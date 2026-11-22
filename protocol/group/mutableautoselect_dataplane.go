package group

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/adapter"
)

// Five seconds is a provisional margin below the default 10s DNS/NTP/STUN idle
// timeout. Delayed resets after partial replies can also be excused.
const defaultDataPlaneResetQuiet = 5 * time.Second

const defaultFirstResponseTimeout = 10 * time.Second

// earlyConfirmAfter is how long a TCP conn waits for its first response
// before its member is probed. Long before a caller gives up, it is long
// enough that a healthy member has usually answered.
const earlyConfirmAfter = 1500 * time.Millisecond

// defaultUDPUnansweredAfter is how long a UDP handle may wait for its first
// datagram before it counts as unanswered. A UDP exchange is answered within
// a round trip or two, and a handle often stays open long after its caller
// gave up, so the wait is judged while the handle is open.
const defaultUDPUnansweredAfter = 1500 * time.Millisecond

// defaultUnansweredAfter is how long a conn must have waited, for its first
// response or for more data, before a local close counts as the caller giving
// up on it. Shorter waits are as likely to be a caller abandoning a request
// for its own reasons.
const defaultUnansweredAfter = 2 * time.Second

type dataPlaneHooks struct {
	// onFailure gets the error that caused a reset; a stall or a missing
	// response has none.
	onFailure func(kind adapter.UserFailureKind, cause error)
	// onError runs on the IO goroutine, outside the watchdog's lock.
	onError    func(err error, state dataPlaneIO, excused bool)
	onActivity func(time.Time)
	// onResponse runs on the IO path when the conn reads data at at, from
	// dest when known: for a TCP conn on its first read, for a UDP handle at
	// most once a second.
	onResponse func(at time.Time, dest string)
	// onUnanswered runs on its own goroutine when the conn, or for UDP one
	// of its exchanges, went unanswered while its caller had been waiting
	// for data from dest since waitingSince.
	onUnanswered func(waitingSince time.Time, dest string)
	// onClose runs once, on the conn's first Close.
	onClose func()
	// onSlowResponse runs once, on its own goroutine, if the conn still has
	// no first response slowResponseAfter after its first write.
	onSlowResponse func()
	// detached, once set, means the conn's member was removed: the hooks the
	// group installed no longer report on it. Nil for a conn without them.
	detached *atomic.Bool
	// onReset runs synchronously when a reset is charged, before onFailure
	// is dispatched on its own goroutine, so a caller can note the reset
	// before the IO error reaches the client. It runs on the IO path, so it
	// must not wait on IO or take a lock that path holds; short critical
	// sections are fine.
	onReset func()
}

// makeHooks records chargeable failures and starts recovery. Only primary TCP
// resets and unanswered writes trigger selection confirmation.
func (s *MutableAutoSelect) makeHooks(outerTag, network string, route routeKind, generation uint64, trialFor string) dataPlaneHooks {
	confirms := network == N.NetworkTCP && route == primaryRoute
	opened := time.Now()
	detached := new(atomic.Bool)
	// A trial's conn reports a failure once, however many ways it fails,
	// and again only after it has answered since.
	var trialFailed atomic.Bool
	noteTrialFailed := func() {
		if trialFailed.CompareAndSwap(false, true) {
			s.noteTrialFailed(outerTag, network, trialFor, generation)
		}
	}
	hooks := dataPlaneHooks{
		detached: detached,
		onUnanswered: func(waitingSince time.Time, dest string) {
			if detached.Load() {
				return
			}
			if route == trialRoute {
				noteTrialFailed()
			}
			// A UDP handle carries many exchanges; each counts as a conn
			// opened when its wait began.
			exchangeOpened := opened
			if network == N.NetworkUDP {
				exchangeOpened = waitingSince
			}
			if s.chargeable(outerTag, route) {
				s.noteUnanswered(outerTag, generation, network, dest, exchangeOpened, waitingSince)
			} else if network == N.NetworkUDP {
				s.noteUnansweredOffSelection(outerTag, generation, dest, exchangeOpened, waitingSince)
			}
		},
		onFailure: func(kind adapter.UserFailureKind, cause error) {
			if !detached.Load() && route == trialRoute {
				noteTrialFailed()
			}
			if detached.Load() || !s.chargeable(outerTag, route) {
				return
			}
			if !s.recordUserFailure(outerTag, kind, generation, opened, network, cause) {
				return
			}
			if confirms && (kind == adapter.UserFailureReset || kind == adapter.UserFailureNoResponse) {
				s.requestFailureConfirmation(outerTag, "", generation)
			}
		},
		onError: func(err error, state dataPlaneIO, excused bool) {
			if !detached.Load() && failedLocally(err) {
				if route == trialRoute {
					noteTrialFailed()
				} else {
					s.noteSelectionFailedLocally(outerTag, network, generation)
				}
			}
			decision := "candidate"
			if excused {
				decision = "excused"
			}
			last := "none"
			if state.hasIO {
				last = "read"
				if state.lastWasWrite {
					last = "write"
				}
			}
			s.logger.Debug("data-plane error: tag=", outerTag, " decision=", decision,
				" err=", err, " quiet=", state.quiet, " last=", last, " proven=", state.proven)
		},
		onActivity: s.noteDataActivity,
	}
	if confirms {
		hooks.onSlowResponse = func() {
			if !detached.Load() {
				s.requestEarlyConfirmation(outerTag, generation)
			}
		}
	}
	hooks.onResponse = func(at time.Time, dest string) {
		if detached.Load() {
			return
		}
		if route == trialRoute {
			trialFailed.Store(false)
			s.noteTrialAnswered(outerTag, network, generation, trialFor)
		}
		s.noteUserResponse(outerTag, generation, network, dest, opened, at)
	}
	return hooks
}

type dataPlaneIO struct {
	hasIO        bool
	lastWasWrite bool
	quiet        time.Duration
	proven       bool
}

// dataPlaneWatchdog detects unanswered writes and mid-stream stalls.
// Idle expiry counts only after provedReadBytes have arrived and the last
// non-empty IO was a write, so unused keep-alive connections are not charged.
//
// A non-zero firstResponseTimeout starts when the first non-empty write begins,
// independently of provedReadBytes. Read bytes or a non-timeout read error
// (including EOF) disarm it. Set firstResponseTimeout before the first IO.
//
// With a non-zero unansweredAfter as well, a conn is reported unanswered when
// it reads EOF after writing but before its first response, or when it is
// closed locally, before reading EOF, after waiting at least unansweredAfter
// since its first write began for a first response. With reportQuiet, a local close
// at least unansweredAfter after the last IO, once data has arrived, is
// reported too. With unansweredWhileOpen, for UDP, each exchange is
// watched: a datagram sent with no reply since the previous one starts a
// wait, reported if nothing is read within unansweredAfter, and the next
// datagram sent after a report starts another. Set these before the first
// IO.
type dataPlaneWatchdog struct {
	idle                 time.Duration
	firstResponseTimeout time.Duration
	unansweredAfter      time.Duration
	slowResponseAfter    time.Duration
	reportQuiet          bool
	unansweredWhileOpen  bool
	hooks                dataPlaneHooks
	provedReadBytes      uint64
	born                 time.Time
	ioMu                 sync.Mutex
	lastIO               time.Time   // guarded by ioMu
	lastWasWrite         bool        // guarded by ioMu
	firstResponseTimer   *time.Timer // guarded by ioMu
	slowResponseTimer    *time.Timer // guarded by ioMu
	firstWriteAt         time.Time   // guarded by ioMu
	unansweredTimer      *time.Timer // guarded by ioMu
	// The current UDP exchange wait, guarded by ioMu: when it began and its
	// destination, the latest destination waited on, whether an unanswered
	// report awaits a reply, when a reply was last passed to onResponse, and
	// the sources a reply has been passed on from. A report leaves the
	// handle's other detection armed, since the group may not count it.
	awaitingSince       time.Time
	awaitingDest        string
	lastDest            string
	reportedSinceAnswer bool
	// reportedDest is the destination of the latest unanswered report.
	reportedDest     string
	lastAnswerNotify time.Time
	notifiedSources  map[string]struct{}
	// The current wait's id; its sends still under way and how many joined
	// it; the join order of the earliest send that sent, or -1; and the
	// latest destination waited on before it began.
	waitID       uint64
	waitSends    int
	waitJoins    int
	waitSentSeq  int
	waitPrevDest string
	readBytes    atomic.Uint64
	responded    atomic.Bool
	answered     atomic.Bool // set by the first read of data
	peerClosed   atomic.Bool // set by a read of EOF
	closed       atomic.Bool
	proven       atomic.Bool
	stalled      atomic.Bool
	fired        atomic.Bool
	timer        *time.Timer
	closeOnce    sync.Once
}

func (w *dataPlaneWatchdog) init(idle time.Duration, provedReadBytes uint64, hooks dataPlaneHooks) {
	w.idle = idle
	w.provedReadBytes = provedReadBytes
	w.hooks = hooks
	w.born = time.Now()
	w.timer = time.AfterFunc(idle, w.fireStall)
}

// isDataPlaneFailure reports whether err should demote the outbound.
// Clean closes, caller cancellation, and timeouts are ignored.
func isDataPlaneFailure(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return false
	}
	return !isTimeout(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func endsQuietRead(isRead bool, n int, state dataPlaneIO) bool {
	return isRead && n == 0 && state.hasIO && !state.lastWasWrite &&
		state.quiet >= defaultDataPlaneResetQuiet
}

func (w *dataPlaneWatchdog) noteIO(n int, err error, isRead bool) {
	w.noteIOFrom(n, err, isRead, M.Socksaddr{})
}

// noteIOFrom is noteIO for a read that reports where its datagram came from.
func (w *dataPlaneWatchdog) noteIOFrom(n int, err error, isRead bool, source M.Socksaddr) {
	var now time.Time
	if n > 0 && err == nil {
		now = time.Now()
		// Before the stalled check: a stall leaves the conn open, and
		// traffic that resumes on it is still use of the group.
		if w.hooks.onActivity != nil {
			w.hooks.onActivity(now)
		}
	}
	// Before the stalled check too: an answer that arrives after the conn
	// was reported unanswered still shows the member delivering.
	if isRead && n > 0 {
		at := now
		if at.IsZero() {
			at = time.Now()
		}
		first := w.answered.CompareAndSwap(false, true)
		switch {
		case w.unansweredWhileOpen:
			if dest, notify := w.noteDatagramReceived(at, source); notify && w.hooks.onResponse != nil {
				w.hooks.onResponse(at, dest)
			}
		case first && w.hooks.onResponse != nil:
			// The hook can block on the group's lock, so the response
			// timers stop first.
			if w.needsResponseTracking() {
				w.markResponded()
			}
			w.hooks.onResponse(at, "")
		}
	}
	// Short-circuit once stalled: a late noteIO must not re-arm the
	// timer or attribute a failure after the conn is logically gone.
	if w.stalled.Load() {
		return
	}
	if !isRead && n > 0 && w.unansweredAfter > 0 {
		w.noteFirstWrite()
	}
	if isRead && n == 0 && errors.Is(err, io.EOF) {
		w.peerClosed.Store(true)
	}
	if w.needsResponseTracking() {
		switch {
		case isRead && n == 0 && errors.Is(err, io.EOF) && w.unansweredAfter > 0 && w.wrote():
			if w.stalled.CompareAndSwap(false, true) {
				w.reportUnanswered(w.firstWrite(), "")
			}
			return
		case isRead && (n > 0 || (err != nil && !isTimeout(err))):
			w.markResponded()
		case !isRead && n > 0:
			w.startFirstResponseTimer()
		}
	}
	if isDataPlaneFailure(err) {
		state := w.snapshotIO()
		excused := endsQuietRead(isRead, n, state)
		if !excused {
			w.fireResetFailure(err)
		}
		if w.hooks.onError != nil {
			w.hooks.onError(err, state, excused)
		}
		return
	}
	// Ignore empty I/O and benign terminal conditions.
	if n <= 0 || err != nil {
		return
	}
	// Publish the IO state before re-arming the stall timer.
	w.ioMu.Lock()
	w.lastIO = now
	w.lastWasWrite = !isRead
	w.ioMu.Unlock()
	w.timer.Reset(w.idle)
	if !isRead {
		return
	}
	// Mark the conn proven once cumulative Read bytes cross the
	// threshold. provedReadBytes==0 keeps the legacy "any non-empty Read
	// proves" behavior for tests that don't care about the threshold;
	// production callers always pass a non-zero default.
	if w.proven.Load() {
		return
	}
	total := w.readBytes.Add(uint64(n))
	if total >= w.provedReadBytes {
		w.proven.Store(true)
	}
}

func (w *dataPlaneWatchdog) snapshotIO() dataPlaneIO {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	last := w.lastIO
	hasIO := !last.IsZero()
	if !hasIO {
		last = w.born
	}
	return dataPlaneIO{
		hasIO:        hasIO,
		lastWasWrite: w.lastWasWrite,
		quiet:        time.Since(last),
		proven:       w.proven.Load(),
	}
}

// closeWatchdog sets stalled=true before Stop so any concurrent noteIO
// short-circuits and any concurrent fireStall CAS-fails — late I/O can't
// deliver a phantom onFailure after Close.
func (w *dataPlaneWatchdog) closeWatchdog() (firstClose bool) {
	w.closeOnce.Do(func() {
		w.stalled.Store(true)
		w.closed.Store(true)
		w.timer.Stop()
		w.stopFirstResponseTimer()
		w.stopUnansweredTimer()
		if w.hooks.onClose != nil {
			w.hooks.onClose()
		}
		firstClose = true
	})
	return firstClose
}

func (w *dataPlaneWatchdog) needsResponseTracking() bool {
	return w.firstResponseTimeout > 0 && !w.responded.Load()
}

func (w *dataPlaneWatchdog) startFirstResponseTimer() {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.firstResponseTimer != nil || w.responded.Load() || w.stalled.Load() {
		return
	}
	w.firstResponseTimer = time.AfterFunc(w.firstResponseTimeout, w.fireNoResponse)
	if w.slowResponseAfter > 0 && w.hooks.onSlowResponse != nil {
		w.slowResponseTimer = time.AfterFunc(w.slowResponseAfter, w.fireSlowResponse)
	}
}

func (w *dataPlaneWatchdog) fireSlowResponse() {
	w.ioMu.Lock()
	fire := !w.responded.Load() && !w.stalled.Load()
	w.ioMu.Unlock()
	if fire {
		go w.hooks.onSlowResponse()
	}
}

// awaitingFirstResponse reports whether the conn has had no response and
// wrote at least minWait ago; a zero minWait includes a conn that hasn't
// written yet. Data counts as soon as a read returns it, before the read's
// hooks run.
func (w *dataPlaneWatchdog) awaitingFirstResponse(now time.Time, minWait time.Duration) bool {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.responded.Load() || w.answered.Load() {
		return false
	}
	return minWait == 0 || (!w.firstWriteAt.IsZero() && now.Sub(w.firstWriteAt) >= minWait)
}

// noteWriteStarting starts waiting for a response as a write begins, since a
// first write can block on a deferred handshake while its caller waits.
func (w *dataPlaneWatchdog) noteWriteStarting() {
	if w.stalled.Load() {
		return
	}
	if w.unansweredAfter > 0 {
		w.noteFirstWrite()
	}
	if w.needsResponseTracking() {
		w.startFirstResponseTimer()
	}
}

func (w *dataPlaneWatchdog) noteFirstWrite() {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
	}
}

// datagramWait identifies the wait a send began or joined, the send's place
// in it, when it began and where to, so its outcome can be recorded against
// the wait.
type datagramWait struct {
	id    uint64
	seq   int
	since time.Time
	dest  string
}

// beginDatagramWait starts a wait for a reply to a datagram about to be sent
// to dest, unless one is already running. It runs before the send, so a reply
// read before the send returns still ends the wait.
func (w *dataPlaneWatchdog) beginDatagramWait(dest M.Socksaddr) datagramWait {
	// An idle stall leaves the handle open, so exchanges are still watched.
	if !w.unansweredWhileOpen || w.closed.Load() {
		return datagramWait{}
	}
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	// Unwrapped so an IPv4 destination has one spelling however the
	// datagram was addressed.
	to := w.lastDest
	if dest = dest.Unwrap(); dest.IsValid() {
		to = dest.String()
	}
	if !w.awaitingSince.IsZero() {
		w.waitSends++
		w.waitJoins++
		return datagramWait{id: w.waitID, seq: w.waitJoins, since: time.Now(), dest: to}
	}
	w.waitID++
	started := datagramWait{id: w.waitID, since: time.Now(), dest: to}
	w.awaitingSince, w.waitSends, w.waitJoins, w.waitSentSeq, w.waitPrevDest = started.since, 1, 0, -1, w.lastDest
	w.lastDest, w.awaitingDest = to, to
	if w.unansweredTimer == nil {
		w.unansweredTimer = time.AfterFunc(w.unansweredAfter, w.fireUnansweredWhileOpen)
	} else {
		w.unansweredTimer.Reset(w.unansweredAfter)
	}
	return started
}

// endDatagramSend records whether a send that began or joined wait sent its
// datagram. A wait still running is for the earliest send that sent: its
// destination, from when it began. Once every send has ended and none sent,
// the wait is undone.
func (w *dataPlaneWatchdog) endDatagramSend(wait datagramWait, sent bool) {
	if wait.id == 0 {
		return
	}
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.awaitingSince.IsZero() || wait.id != w.waitID {
		return
	}
	w.waitSends--
	if sent && (w.waitSentSeq < 0 || wait.seq < w.waitSentSeq) {
		w.waitSentSeq = wait.seq
		w.awaitingSince, w.awaitingDest, w.lastDest = wait.since, wait.dest, wait.dest
		w.unansweredTimer.Reset(w.unansweredAfter - time.Since(wait.since))
		return
	}
	if w.waitSends > 0 || w.waitSentSeq >= 0 {
		return
	}
	w.awaitingSince, w.lastDest = time.Time{}, w.waitPrevDest
	w.unansweredTimer.Stop()
}

// noteDatagramReceived ends the current wait, since the handle delivered,
// and reports whether to pass the reply on and the address it came from,
// source, falling back to the destination waited on when source is unknown.
// A reply credits its destination only if it comes back addressed as it was
// sent, as sing-box's NAT-wrapped conns are; otherwise detection for that
// destination stays quiet rather than misfiring. The first reply from each of
// up to maxUDPAnsweredDestinations sources is passed on; others at most once
// a second.
func (w *dataPlaneWatchdog) noteDatagramReceived(at time.Time, source M.Socksaddr) (dest string, notify bool) {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if source = source.Unwrap(); source.IsValid() {
		dest = source.String()
	} else if dest = w.lastDest; !w.awaitingSince.IsZero() {
		dest = w.awaitingDest
	}
	// unansweredAfter exceeds this limit, so a reply after an unanswered
	// report is always passed on, ending the streak it fed.
	notify = w.lastAnswerNotify.IsZero() || at.Sub(w.lastAnswerNotify) >= time.Second
	// Past the limit, as on a peer-to-peer socket, new sources share the
	// once-a-second pass, which bounds the group lock taken per handle.
	if _, seen := w.notifiedSources[dest]; !seen && dest != "" && len(w.notifiedSources) < maxUDPAnsweredDestinations {
		if w.notifiedSources == nil {
			w.notifiedSources = make(map[string]struct{})
		}
		w.notifiedSources[dest] = struct{}{}
		notify = true
	}
	w.awaitingSince, w.reportedSinceAnswer = time.Time{}, false
	if w.unansweredTimer != nil {
		w.unansweredTimer.Stop()
	}
	if notify {
		w.lastAnswerNotify = at
	}
	return dest, notify
}

func (w *dataPlaneWatchdog) fireUnansweredWhileOpen() {
	w.ioMu.Lock()
	since, dest := w.awaitingSince, w.awaitingDest
	// Until one of its sends has sent, the wait may be for nothing; a send
	// that does re-arms the timer.
	report := !since.IsZero() && w.waitSentSeq >= 0 && !w.closed.Load() && time.Since(since) >= w.unansweredAfter
	if report {
		w.awaitingSince, w.reportedSinceAnswer, w.reportedDest = time.Time{}, true, dest
	}
	w.ioMu.Unlock()
	if report && w.hooks.onUnanswered != nil {
		go w.hooks.onUnanswered(since, dest)
	}
}

// awaitingReply reports whether the handle's latest exchange went
// unanswered: reported with no reply since, or waiting at least
// unansweredAfter since a send that went out.
func (w *dataPlaneWatchdog) awaitingReply(now time.Time) bool {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	return w.reportedSinceAnswer ||
		(!w.awaitingSince.IsZero() && w.waitSentSeq >= 0 && now.Sub(w.awaitingSince) >= w.unansweredAfter)
}

// awaitingReplyFrom is awaitingReply for an exchange with dest: one
// reported unanswered, or waiting at least unansweredAfter since a send
// that went out.
func (w *dataPlaneWatchdog) awaitingReplyFrom(now time.Time, dest string) bool {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	return (w.reportedSinceAnswer && w.reportedDest == dest) ||
		(!w.awaitingSince.IsZero() && w.waitSentSeq >= 0 && w.awaitingDest == dest && now.Sub(w.awaitingSince) >= w.unansweredAfter)
}

func (w *dataPlaneWatchdog) stopUnansweredTimer() {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.unansweredTimer != nil {
		w.unansweredTimer.Stop()
	}
}

func (w *dataPlaneWatchdog) wrote() bool {
	return !w.firstWrite().IsZero()
}

func (w *dataPlaneWatchdog) firstWrite() time.Time {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	return w.firstWriteAt
}

// reportUnanswered runs onUnanswered on its own goroutine, since it is
// called on the IO path. The caller has set stalled.
func (w *dataPlaneWatchdog) reportUnanswered(waitingSince time.Time, dest string) {
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.hooks.onUnanswered != nil {
		go w.hooks.onUnanswered(waitingSince, dest)
	}
}

// closeUnanswered reports a local close after waiting at least
// unansweredAfter for data: for a first response since the first write, or
// for more data since the last IO once some has arrived. An idle keep-alive
// conn closing looks the same; see noteUnanswered.
func (w *dataPlaneWatchdog) closeUnanswered() {
	if w.unansweredAfter <= 0 || w.peerClosed.Load() {
		return
	}
	w.ioMu.Lock()
	var since time.Time
	dest := w.awaitingDest
	switch {
	case w.unansweredWhileOpen:
		if w.waitSentSeq >= 0 {
			since = w.awaitingSince
		}
	case !w.answered.Load():
		since = w.firstWriteAt
	case w.reportQuiet:
		since = w.lastIO
	}
	report := !since.IsZero() && time.Since(since) >= w.unansweredAfter && w.stalled.CompareAndSwap(false, true)
	if report && w.unansweredWhileOpen {
		// The open-handle timer must not report this wait as well.
		w.awaitingSince = time.Time{}
	}
	w.ioMu.Unlock()
	if report {
		w.reportUnanswered(since, dest)
	}
}

// markResponded and fireNoResponse decide under ioMu, so a reply recorded
// before the timeout decision is never charged.
func (w *dataPlaneWatchdog) markResponded() {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	w.responded.Store(true)
	if w.firstResponseTimer != nil {
		w.firstResponseTimer.Stop()
	}
	if w.slowResponseTimer != nil {
		w.slowResponseTimer.Stop()
	}
}

func (w *dataPlaneWatchdog) stopFirstResponseTimer() {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.firstResponseTimer != nil {
		w.firstResponseTimer.Stop()
	}
	if w.slowResponseTimer != nil {
		w.slowResponseTimer.Stop()
	}
}

func (w *dataPlaneWatchdog) fireNoResponse() {
	w.ioMu.Lock()
	charge := !w.responded.Load() && w.stalled.CompareAndSwap(false, true)
	w.ioMu.Unlock()
	if !charge {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.hooks.onFailure != nil {
		w.hooks.onFailure(adapter.UserFailureNoResponse, nil)
	}
}

// fireStall is the idle-timer callback: it demotes a conn that went quiet
// mid-stream. It fires only on a proven conn whose last non-empty IO was a
// Write.
func (w *dataPlaneWatchdog) fireStall() {
	if !w.proven.Load() {
		return
	}
	if !w.snapshotIO().lastWasWrite {
		return
	}
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.hooks.onFailure != nil {
		w.hooks.onFailure(adapter.UserFailureStall, nil)
	}
}

// fireResetFailure attributes a non-excused error once, even on an unproven conn.
func (w *dataPlaneWatchdog) fireResetFailure(cause error) {
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.hooks.onReset != nil {
		w.hooks.onReset()
	}
	if w.hooks.onFailure != nil {
		// run onFailure in a goroutine to avoid deadlocks: the callback may call back
		// into the selector, which may hold locks that the data-plane IO path also needs.
		go w.hooks.onFailure(adapter.UserFailureReset, cause)
	}
}

// dataPlaneStream monitors stream IO for transport failures and unanswered writes.
type dataPlaneStream struct {
	net.Conn
	dataPlaneWatchdog
}

func newDataPlaneStream(c net.Conn, idle time.Duration, provedReadBytes uint64, hooks dataPlaneHooks) *dataPlaneStream {
	d := &dataPlaneStream{Conn: c}
	d.init(idle, provedReadBytes, hooks)
	return d
}

func (d *dataPlaneStream) Read(p []byte) (int, error) {
	n, err := d.Conn.Read(p)
	d.noteIO(n, err, true)
	return n, err
}

func (d *dataPlaneStream) Write(p []byte) (int, error) {
	if len(p) > 0 {
		d.noteWriteStarting()
	}
	n, err := d.Conn.Write(p)
	d.noteIO(n, err, false)
	return n, err
}

// Close is idempotent: sing-box's connection lifecycle frequently
// double-closes, so the underlying Close runs at most once.
func (d *dataPlaneStream) Close() error {
	d.closeUnanswered()
	if !d.closeWatchdog() {
		return nil
	}
	return d.Conn.Close()
}

// Upstream lets common.Cast descend through the wrapper to find feature
// interfaces (EarlyConn, VectorisedConn, ReadWaiter, ...) on the inner
// outbound; without this, fast paths downstream silently disable.
func (d *dataPlaneStream) Upstream() any { return d.Conn }

// dataPlanePacket wraps a net.PacketConn; same contract as dataPlaneStream.
type dataPlanePacket struct {
	net.PacketConn
	dataPlaneWatchdog
	writer N.PacketWriter
}

func newDataPlanePacket(
	c net.PacketConn,
	idle time.Duration,
	provedReadBytes uint64,
	hooks dataPlaneHooks,
) *dataPlanePacket {
	d := &dataPlanePacket{PacketConn: c, writer: bufio.NewPacketConn(c)}
	d.init(idle, provedReadBytes, hooks)
	return d
}

func (d *dataPlanePacket) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := d.PacketConn.ReadFrom(p)
	var source M.Socksaddr
	if n > 0 && err == nil && d.unansweredWhileOpen {
		source = M.SocksaddrFromNet(addr)
	}
	d.noteIOFrom(n, err, true, source)
	return n, addr, err
}

func (d *dataPlanePacket) WriteTo(p []byte, addr net.Addr) (int, error) {
	var started datagramWait
	if len(p) > 0 {
		started = d.beginDatagramWait(M.SocksaddrFromNet(addr))
	}
	n, err := d.PacketConn.WriteTo(p, addr)
	d.endDatagramSend(started, err == nil && n > 0)
	d.noteIO(n, err, false)
	return n, err
}

// ReadPacket reads through ReadFrom, so the watchdog sees the read.
func (d *dataPlanePacket) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(d)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr).Unwrap(), nil
}

// WritePacket passes domain-name destinations through; bufio's
// net.PacketConn adapter would strip them to IP-only.
func (d *dataPlanePacket) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	n := buffer.Len()
	var started datagramWait
	if n > 0 {
		started = d.beginDatagramWait(destination)
	}
	err := d.writer.WritePacket(buffer, destination)
	d.endDatagramSend(started, err == nil)
	d.noteIO(n, err, false)
	return err
}

func (d *dataPlanePacket) Close() error {
	d.closeUnanswered()
	if !d.closeWatchdog() {
		return nil
	}
	return d.PacketConn.Close()
}

func (d *dataPlanePacket) Upstream() any { return d.PacketConn }

var (
	_ net.Conn        = (*dataPlaneStream)(nil)
	_ N.NetPacketConn = (*dataPlanePacket)(nil)
)
