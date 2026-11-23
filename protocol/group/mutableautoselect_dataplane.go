package group

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
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

// connHooks receives a watched conn's events.
type connHooks interface {
	// failure gets the error that caused a reset; a stall or a missing
	// response has none.
	failure(kind adapter.UserFailureKind, cause error)
	// ioError runs on the IO goroutine, outside the watchdog's lock.
	ioError(err error, state dataPlaneIO, excused bool)
	activity(at time.Time)
	// response runs on the IO path when the conn reads data at at, from dest
	// when known: for a TCP conn on its first read, for a UDP handle at most
	// once a second.
	response(at time.Time, dest string)
	// unanswered runs on its own goroutine when the conn, or for UDP one of
	// its exchanges, went unanswered while its caller had been waiting for
	// data from dest since waitingSince.
	unanswered(waitingSince time.Time, dest string)
	// closed runs once, on the conn's first Close.
	closed()
	// slowResponse runs once, on its own goroutine, if the conn still has no
	// first response slowResponseAfter after its first write.
	slowResponse()
	// reset runs synchronously when a reset is charged, before failure is
	// dispatched on its own goroutine, so a caller can note the reset before
	// the IO error reaches the client. It runs on the IO path, so it must not
	// wait on IO or take a lock that path holds; short critical sections are
	// fine.
	reset()
}

// dataPlaneHooks is a connHooks of functions; a nil one does nothing.
type dataPlaneHooks struct {
	onFailure      func(kind adapter.UserFailureKind, cause error)
	onError        func(err error, state dataPlaneIO, excused bool)
	onActivity     func(time.Time)
	onResponse     func(at time.Time, dest string)
	onUnanswered   func(waitingSince time.Time, dest string)
	onClose        func()
	onSlowResponse func()
	onReset        func()
}

func (h dataPlaneHooks) failure(kind adapter.UserFailureKind, cause error) {
	if h.onFailure != nil {
		h.onFailure(kind, cause)
	}
}

func (h dataPlaneHooks) ioError(err error, state dataPlaneIO, excused bool) {
	if h.onError != nil {
		h.onError(err, state, excused)
	}
}

func (h dataPlaneHooks) activity(at time.Time) {
	if h.onActivity != nil {
		h.onActivity(at)
	}
}

func (h dataPlaneHooks) response(at time.Time, dest string) {
	if h.onResponse != nil {
		h.onResponse(at, dest)
	}
}

func (h dataPlaneHooks) unanswered(waitingSince time.Time, dest string) {
	if h.onUnanswered != nil {
		h.onUnanswered(waitingSince, dest)
	}
}

func (h dataPlaneHooks) closed() {
	if h.onClose != nil {
		h.onClose()
	}
}

func (h dataPlaneHooks) slowResponse() {
	if h.onSlowResponse != nil {
		h.onSlowResponse()
	}
}

func (h dataPlaneHooks) reset() {
	if h.onReset != nil {
		h.onReset()
	}
}

// connReport reports a conn's events to the group: one on network through
// origin, set up at opened. It records chargeable failures and starts
// recovery; only primary TCP resets and unanswered writes trigger selection
// confirmation.
type connReport struct {
	s       *MutableAutoSelect
	origin  connOrigin
	network string
	opened  time.Time
	// trialFailed is set once a trial's conn reported failing, until it
	// answers again, so it reports a failure once however many ways it fails.
	trialFailed atomic.Bool
	// answered is set once a trial's UDP handle answered.
	answered atomic.Bool
	// stream or packet is the conn, untracked when it closes; a non-nil
	// release then runs too. A trial's packet that never answered is closed
	// once it goes unanswered.
	stream  *dataPlaneStream
	packet  *dataPlanePacket
	release func()
}

func (s *MutableAutoSelect) makeHooks(origin connOrigin, network string) *connReport {
	return &connReport{s: s, origin: origin, network: network, opened: time.Now()}
}

func (r *connReport) confirms() bool {
	return r.network == N.NetworkTCP && r.origin.route == primaryRoute
}

func (r *connReport) noteTrialFailed() {
	if r.origin.route == trialRoute && r.trialFailed.CompareAndSwap(false, true) {
		r.s.noteTrialFailed(r.origin, r.network)
	}
}

func (r *connReport) unanswered(waitingSince time.Time, dest string) {
	r.noteTrialFailed()
	// A UDP handle carries many exchanges; each counts as a conn opened when
	// its wait began.
	exchangeOpened := r.opened
	if r.network == N.NetworkUDP {
		exchangeOpened = waitingSince
	}
	if r.s.chargeable(r.origin.out.Tag(), r.origin.route) {
		r.s.noteUnanswered(r.origin, r.network, dest, exchangeOpened, waitingSince)
	} else if r.network == N.NetworkUDP {
		r.s.noteUnansweredOffSelection(r.origin, dest, exchangeOpened, waitingSince)
	}
	// A trial that never answered closes, so an app holding its socket sets
	// up again rather than staying on a member that failed; one that
	// answered serves its app.
	if r.packet != nil && r.origin.route == trialRoute && !r.answered.Load() {
		r.packet.Close()
	}
}

func (r *connReport) failure(kind adapter.UserFailureKind, cause error) {
	r.noteTrialFailed()
	if !r.s.chargeable(r.origin.out.Tag(), r.origin.route) || !r.s.recordUserFailure(r.origin, kind, r.opened, r.network, cause) {
		return
	}
	if r.confirms() && (kind == adapter.UserFailureReset || kind == adapter.UserFailureNoResponse) {
		r.s.requestFailureConfirmation(r.origin, "")
	}
}

func (r *connReport) ioError(err error, state dataPlaneIO, excused bool) {
	if failedLocally(err) {
		if r.origin.route == trialRoute {
			r.noteTrialFailed()
		} else {
			r.s.noteSelectionSetup(r.origin, r.network, false)
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
	r.s.logger.Debug("data-plane error: tag=", r.origin.out.Tag(), " decision=", decision,
		" err=", err, " quiet=", state.quiet, " last=", last, " proven=", state.proven)
}

func (r *connReport) activity(at time.Time) { r.s.noteDataActivity(at) }

func (r *connReport) response(at time.Time, dest string) {
	if r.origin.route == trialRoute {
		r.answered.Store(true)
		r.trialFailed.Store(false)
		r.s.noteTrialAnswered(r.origin, r.network)
	}
	r.s.noteUserResponse(r.origin, r.network, dest, r.opened, at)
}

func (r *connReport) closed() {
	tag := r.origin.out.Tag()
	switch {
	case r.stream != nil:
		untrackConn(r.s, &r.s.tcpStreams, tag, r.stream)
	case r.packet != nil:
		untrackConn(r.s, &r.s.udpHandles, tag, r.packet)
	}
	if r.release != nil {
		r.release()
	}
}

func (r *connReport) slowResponse() {
	if r.confirms() {
		r.s.requestEarlyConfirmation(r.origin)
	}
}

func (r *connReport) reset() {}

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
// since its first write began for a first response, or at least
// unansweredAfter after the last IO once data has arrived. With
// unansweredWhileOpen, for UDP, each exchange is
// watched: a datagram sent with no reply since the previous one starts a
// wait, reported if nothing is read within unansweredAfter, and the next
// datagram sent after a report starts another. Set these before the first
// IO.
type dataPlaneWatchdog struct {
	idle                 time.Duration
	firstResponseTimeout time.Duration
	unansweredAfter      time.Duration
	slowResponseAfter    time.Duration
	unansweredWhileOpen  bool
	hooks                connHooks
	provedReadBytes      uint64
	born                 time.Time
	ioMu                 sync.Mutex
	lastIO               time.Time // guarded by ioMu
	lastWasWrite         bool      // guarded by ioMu
	// Guarded by ioMu: the stall timer, created once the conn is proven,
	// fires idle after the IO that armed it, and one that finds IO since
	// waits out the rest; stallArmed is whether it is pending.
	stallTimer *time.Timer
	stallArmed bool
	// Guarded by ioMu: the response timer fires slowResponseAfter after the
	// first write while slowPending, then at noResponseAt.
	responseTimer *time.Timer
	slowPending   bool
	noResponseAt  time.Time
	firstWriteAt  time.Time // guarded by ioMu
	// wroteFirst is set once firstWriteAt is.
	wroteFirst atomic.Bool
	// udp is a UDP handle's exchanges, nil for a stream.
	udp        *datagramExchanges
	readBytes  atomic.Uint64
	responded  atomic.Bool
	answered   atomic.Bool // set by the first read of data
	peerClosed atomic.Bool // set by a read of EOF
	closed     atomic.Bool
	proven     atomic.Bool
	stalled    atomic.Bool
	fired      atomic.Bool
	closeOnce  sync.Once
}

// datagramExchanges is a UDP handle's exchanges, guarded by its watchdog's
// ioMu.
type datagramExchanges struct {
	// The current wait: when it began and its destination, the latest
	// destination waited on, whether an unanswered report awaits a reply,
	// when a reply was last passed to response, and the sources a reply has
	// been passed on from. A report leaves the handle's other detection
	// armed, since the group may not count it.
	awaitingSince       time.Time
	awaitingDest        string
	lastDest            string
	reportedSinceAnswer bool
	// reportedDest is the destination of the latest unanswered report.
	reportedDest     string
	lastAnswerNotify time.Time
	notifiedSources  sourceSet
	// The current wait's id; its sends still under way and how many joined
	// it; the join order of the earliest send that sent, or -1; and the
	// latest destination waited on before it began.
	waitID           uint64
	waitSends        int
	waitJoins        int
	waitSentSeq      int
	waitPrevDest     string
	sentTo, readFrom addrName
	// The unanswered timer, once created, is pending until unansweredDue,
	// or not if that is zero. One that fires before its wait is due waits
	// out the rest, so a wait need not re-arm it.
	unansweredTimer *time.Timer
	unansweredDue   time.Time
}

func (w *dataPlaneWatchdog) init(idle time.Duration, provedReadBytes uint64, hooks connHooks) {
	w.idle = idle
	w.provedReadBytes = provedReadBytes
	w.hooks = hooks
	w.born = time.Now()
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
		w.hooks.activity(now)
	}
	// Before the stalled check too: an answer that arrives after the conn
	// was reported unanswered still shows the member delivering.
	if isRead && n > 0 {
		at := now
		if at.IsZero() {
			at = time.Now()
		}
		first := !w.answered.Load() && w.answered.CompareAndSwap(false, true)
		switch {
		case w.unansweredWhileOpen:
			if dest, notify := w.noteDatagramReceived(at, source); notify {
				w.hooks.response(at, dest)
			}
		case first:
			// The hook can block on the group's lock, so the response
			// timers stop first.
			if w.needsResponseTracking() {
				w.markResponded()
			}
			w.hooks.response(at, "")
		}
	}
	// Short-circuit once stalled: a late noteIO must not re-arm the
	// timer or attribute a failure after the conn is logically gone.
	if w.stalled.Load() {
		return
	}
	eof := isRead && n == 0 && errors.Is(err, io.EOF)
	if eof {
		w.peerClosed.Store(true)
	}
	if w.needsResponseTracking() {
		var wroteAt time.Time
		if eof && w.unansweredAfter > 0 {
			wroteAt = w.firstWrite()
		}
		switch {
		case !wroteAt.IsZero():
			if w.stalled.CompareAndSwap(false, true) {
				w.reportUnanswered(wroteAt, "")
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
		w.hooks.ioError(err, state, excused)
		return
	}
	// Ignore empty I/O and benign terminal conditions.
	if n <= 0 || err != nil {
		return
	}
	// Mark the conn proven once cumulative Read bytes cross the
	// threshold. provedReadBytes==0 keeps the legacy "any non-empty Read
	// proves" behavior for tests that don't care about the threshold;
	// production callers always pass a non-zero default.
	if isRead && !w.proven.Load() && w.readBytes.Add(uint64(n)) >= w.provedReadBytes {
		w.proven.Store(true)
	}
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	// An IO that took its time getting here doesn't undo a later one, whose
	// time the stall counts from.
	if !now.Before(w.lastIO) {
		w.lastIO, w.lastWasWrite = now, !isRead
	}
	// Only a proven conn can stall.
	if !w.proven.Load() || w.stallArmed || w.stalled.Load() {
		return
	}
	w.stallArmed = true
	if w.stallTimer == nil {
		w.stallTimer = time.AfterFunc(w.idle, w.fireStall)
	} else {
		w.stallTimer.Reset(w.idle)
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
		w.ioMu.Lock()
		if w.stallTimer != nil {
			w.stallTimer.Stop()
		}
		w.stopResponseTimerLocked()
		if w.udp != nil && w.udp.unansweredTimer != nil {
			w.udp.unansweredTimer.Stop()
		}
		w.ioMu.Unlock()
		w.hooks.closed()
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
	if w.responseTimer != nil || w.responded.Load() || w.stalled.Load() {
		return
	}
	w.noResponseAt = time.Now().Add(w.firstResponseTimeout)
	first := w.firstResponseTimeout
	if w.slowResponseAfter > 0 && w.slowResponseAfter < first {
		first, w.slowPending = w.slowResponseAfter, true
	}
	w.responseTimer = time.AfterFunc(first, w.fireResponseTimer)
}

// fireResponseTimer reports a slow response while one is pending, then a
// missing one.
func (w *dataPlaneWatchdog) fireResponseTimer() {
	w.ioMu.Lock()
	if !w.slowPending {
		w.ioMu.Unlock()
		w.fireNoResponse()
		return
	}
	w.slowPending = false
	fire := !w.responded.Load() && !w.stalled.Load()
	if fire {
		w.responseTimer.Reset(time.Until(w.noResponseAt))
	}
	w.ioMu.Unlock()
	if fire {
		go w.hooks.slowResponse()
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
	if w.wroteFirst.Load() {
		return
	}
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
		w.wroteFirst.Store(true)
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
	x := w.udp
	// Unwrapped so an IPv4 destination has one spelling however the
	// datagram was addressed.
	to := x.lastDest
	if dest = dest.Unwrap(); dest.IsValid() {
		to = x.sentTo.of(dest)
	}
	if !x.awaitingSince.IsZero() {
		x.waitSends++
		x.waitJoins++
		return datagramWait{id: x.waitID, seq: x.waitJoins, since: time.Now(), dest: to}
	}
	x.waitID++
	started := datagramWait{id: x.waitID, since: time.Now(), dest: to}
	x.awaitingSince, x.waitSends, x.waitJoins, x.waitSentSeq, x.waitPrevDest = started.since, 1, 0, -1, x.lastDest
	x.lastDest, x.awaitingDest = to, to
	w.armUnansweredLocked(started.since.Add(w.unansweredAfter))
	return started
}

// armUnansweredLocked has the unanswered timer fire by due. Caller must hold
// ioMu.
func (w *dataPlaneWatchdog) armUnansweredLocked(due time.Time) {
	x := w.udp
	switch {
	case x.unansweredTimer == nil:
		x.unansweredTimer = time.AfterFunc(time.Until(due), w.fireUnansweredWhileOpen)
	case x.unansweredDue.IsZero() || due.Before(x.unansweredDue):
		x.unansweredTimer.Reset(time.Until(due))
	default:
		return
	}
	x.unansweredDue = due
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
	x := w.udp
	if x.awaitingSince.IsZero() || wait.id != x.waitID {
		return
	}
	x.waitSends--
	if sent && (x.waitSentSeq < 0 || wait.seq < x.waitSentSeq) {
		x.waitSentSeq = wait.seq
		x.awaitingSince, x.awaitingDest, x.lastDest = wait.since, wait.dest, wait.dest
		w.armUnansweredLocked(wait.since.Add(w.unansweredAfter))
		return
	}
	if x.waitSends > 0 || x.waitSentSeq >= 0 {
		return
	}
	// The timer, finding no wait, lapses.
	x.awaitingSince, x.lastDest = time.Time{}, x.waitPrevDest
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
	x := w.udp
	if source = source.Unwrap(); source.IsValid() {
		dest = x.readFrom.of(source)
	} else if dest = x.lastDest; !x.awaitingSince.IsZero() {
		dest = x.awaitingDest
	}
	// unansweredAfter exceeds this limit, so a reply after an unanswered
	// report is always passed on, ending the streak it fed.
	notify = x.lastAnswerNotify.IsZero() || at.Sub(x.lastAnswerNotify) >= time.Second
	// Past the limit, as on a peer-to-peer socket, new sources share the
	// once-a-second pass, which bounds the group lock taken per handle.
	if dest != "" && !x.notifiedSources.has(dest) && x.notifiedSources.len() < maxUDPAnsweredDestinations {
		x.notifiedSources.add(dest)
		notify = true
	}
	// The timer, finding no wait, lapses.
	x.awaitingSince = time.Time{}
	x.reportedSinceAnswer = false
	if notify {
		x.lastAnswerNotify = at
	}
	return dest, notify
}

// sourceListMax is how many sources a sourceSet lists before it switches to a
// map: a handle mostly hears from one or two, for which a map costs far more.
const sourceListMax = 8

// sourceSet is a set of source names.
type sourceSet struct {
	list []string
	set  map[string]struct{}
}

func (s *sourceSet) len() int {
	if s.set != nil {
		return len(s.set)
	}
	return len(s.list)
}

func (s *sourceSet) has(name string) bool {
	if s.set != nil {
		_, ok := s.set[name]
		return ok
	}
	return slices.Contains(s.list, name)
}

func (s *sourceSet) add(name string) {
	switch {
	case s.set != nil:
		s.set[name] = struct{}{}
	case len(s.list) < sourceListMax:
		s.list = append(s.list, name)
	default:
		s.set = make(map[string]struct{}, 2*sourceListMax)
		for _, n := range s.list {
			s.set[n] = struct{}{}
		}
		s.set[name] = struct{}{}
		s.list = nil
	}
}

// addrName caches the string form of the address last named, since a
// handle's datagrams mostly go to and come from the same one.
type addrName struct {
	addr M.Socksaddr
	name string
}

func (a *addrName) of(addr M.Socksaddr) string {
	if a.name == "" || a.addr != addr {
		a.addr, a.name = addr, addr.String()
	}
	return a.name
}

func (w *dataPlaneWatchdog) fireUnansweredWhileOpen() {
	w.ioMu.Lock()
	x := w.udp
	x.unansweredDue = time.Time{}
	since, dest := x.awaitingSince, x.awaitingDest
	// Until one of its sends has sent, the wait may be for nothing; a send
	// that does re-arms the timer.
	waiting := !since.IsZero() && x.waitSentSeq >= 0 && !w.closed.Load()
	report := waiting && time.Since(since) >= w.unansweredAfter
	switch {
	case report:
		x.awaitingSince, x.reportedSinceAnswer, x.reportedDest = time.Time{}, true, dest
	case waiting:
		w.armUnansweredLocked(since.Add(w.unansweredAfter))
	}
	w.ioMu.Unlock()
	if report {
		go w.hooks.unanswered(since, dest)
	}
}

// awaitingReply reports whether the handle's latest exchange (with dest,
// unless dest is "") went unanswered: reported with no reply since, or
// waiting at least unansweredAfter since a send that went out.
func (w *dataPlaneWatchdog) awaitingReply(now time.Time, dest string) bool {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	x := w.udp
	return (x.reportedSinceAnswer && (dest == "" || x.reportedDest == dest)) ||
		(!x.awaitingSince.IsZero() && x.waitSentSeq >= 0 && (dest == "" || x.awaitingDest == dest) &&
			now.Sub(x.awaitingSince) >= w.unansweredAfter)
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
	go w.hooks.unanswered(waitingSince, dest)
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
	var dest string
	switch {
	case w.unansweredWhileOpen:
		dest = w.udp.awaitingDest
		if w.udp.waitSentSeq >= 0 {
			since = w.udp.awaitingSince
		}
	case !w.answered.Load():
		since = w.firstWriteAt
	default:
		since = w.lastIO
	}
	report := !since.IsZero() && time.Since(since) >= w.unansweredAfter && w.stalled.CompareAndSwap(false, true)
	if report && w.unansweredWhileOpen {
		// The open-handle timer must not report this wait as well.
		w.udp.awaitingSince = time.Time{}
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
	w.stopResponseTimerLocked()
}

// stopResponseTimerLocked stops the response timer for good, once responded
// or stalled is set, and drops it so an open conn doesn't keep it.
func (w *dataPlaneWatchdog) stopResponseTimerLocked() {
	if w.responseTimer != nil {
		w.responseTimer.Stop()
		w.responseTimer = nil
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
	w.hooks.failure(adapter.UserFailureNoResponse, nil)
}

// fireStall is the stall timer's callback: it demotes a proven conn that went
// quiet mid-stream, idle having passed since its last non-empty IO, a Write.
// It decides under ioMu, which IO takes to record itself, so IO either came
// before and defers the stall or comes after a conn already stalled.
func (w *dataPlaneWatchdog) fireStall() {
	w.ioMu.Lock()
	if quiet := time.Since(w.lastIO); quiet < w.idle && !w.stalled.Load() {
		w.stallTimer.Reset(w.idle - quiet)
		w.ioMu.Unlock()
		return
	}
	w.stallArmed = false
	charge := w.lastWasWrite && w.proven.Load() && w.stalled.CompareAndSwap(false, true) &&
		w.fired.CompareAndSwap(false, true)
	w.ioMu.Unlock()
	if charge {
		w.hooks.failure(adapter.UserFailureStall, nil)
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
	w.hooks.reset()
	// failure runs on its own goroutine to avoid deadlocks: it may call back
	// into the selector, which may hold locks that the data-plane IO path
	// also needs.
	go w.hooks.failure(adapter.UserFailureReset, cause)
}

// dataPlaneStream monitors stream IO for transport failures and unanswered writes.
type dataPlaneStream struct {
	net.Conn
	dataPlaneWatchdog
}

func newDataPlaneStream(c net.Conn, idle time.Duration, provedReadBytes uint64, hooks connHooks) *dataPlaneStream {
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
	exchanges datagramExchanges
	writer    N.PacketWriter
}

func newDataPlanePacket(
	c net.PacketConn,
	idle time.Duration,
	provedReadBytes uint64,
	hooks connHooks,
) *dataPlanePacket {
	d := &dataPlanePacket{PacketConn: c, writer: bufio.NewPacketConn(c)}
	d.init(idle, provedReadBytes, hooks)
	d.udp = &d.exchanges
	return d
}

func (d *dataPlanePacket) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := d.PacketConn.ReadFrom(p)
	var source M.Socksaddr
	if n > 0 && err == nil && d.unansweredWhileOpen {
		source = socksaddrFromNet(addr)
	}
	d.noteIOFrom(n, err, true, source)
	return n, addr, err
}

func (d *dataPlanePacket) WriteTo(p []byte, addr net.Addr) (int, error) {
	var started datagramWait
	if len(p) > 0 {
		started = d.beginDatagramWait(socksaddrFromNet(addr))
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

// socksaddrFromNet is M.SocksaddrFromNet, taking the common *net.UDPAddr
// without its type switches.
func socksaddrFromNet(addr net.Addr) M.Socksaddr {
	if u, ok := addr.(*net.UDPAddr); ok {
		if a := M.SocksaddrFromNetIP(netip.AddrPortFrom(M.AddrFromIP(u.IP), uint16(u.Port))); a.IsValid() {
			return a
		}
	}
	return M.SocksaddrFromNet(addr)
}

var (
	_ net.Conn        = (*dataPlaneStream)(nil)
	_ N.NetPacketConn = (*dataPlanePacket)(nil)
)
