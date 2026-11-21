package group

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service/pause"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
	lConst "github.com/getlantern/lantern-box/constant"
)

func TestDialContext_ExpiredContextSkipsAlternate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var bDials atomic.Int32
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) {
			bDials.Add(1)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return probeResponderConn(), nil
		}
		// Shorter than earlyConfirmAfter, so the deadline ends the dial
		// before a confirmation could.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.Error(t, err)
		synctest.Wait()

		assert.Zero(t, bDials.Load(), "no alternate is dialed with the caller's spent context")
		failures, _ := userFailures(s, "b")
		assert.Empty(t, failures, "the alternate is not charged for the caller's deadline")
	})
}

// slowProbeResponderConn answers one probe request after delay.
func slowProbeResponderConn(delay time.Duration) net.Conn {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		time.Sleep(delay)
		io.WriteString(server, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
	}()
	return client
}

// scriptedConn accepts every write and returns reads queued on reads.
type scriptedConn struct {
	net.Conn
	reads     chan scriptedRead
	closed    chan struct{}
	closeOnce sync.Once
}

type scriptedRead struct {
	data []byte
	err  error
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{reads: make(chan scriptedRead, 4), closed: make(chan struct{})}
}

func (c *scriptedConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *scriptedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	select {
	case r := <-c.reads:
		return copy(p, r.data), r.err
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func TestDataPlaneStream_Unanswered(t *testing.T) {
	type step func(d *dataPlaneStream, c *scriptedConn)
	write := func(d *dataPlaneStream, _ *scriptedConn) { d.Write([]byte("request")) }
	wait := func(dur time.Duration) step {
		return func(*dataPlaneStream, *scriptedConn) { time.Sleep(dur) }
	}
	reply := func(d *dataPlaneStream, c *scriptedConn) {
		c.reads <- scriptedRead{data: []byte("reply")}
		d.Read(make([]byte, 8))
	}
	eof := func(d *dataPlaneStream, c *scriptedConn) {
		c.reads <- scriptedRead{err: io.EOF}
		d.Read(make([]byte, 8))
	}
	closeConn := func(d *dataPlaneStream, _ *scriptedConn) { d.Close() }

	tests := []struct {
		name       string
		steps      []step
		unanswered int32
		answered   int32
	}{
		{"closed after waiting", []step{write, wait(3 * time.Second), closeConn}, 1, 0},
		{"closed before unansweredAfter", []step{write, wait(time.Second), closeConn}, 0, 0},
		{"closed right after a reply", []step{write, reply, closeConn}, 0, 1},
		{"closed after a reply and a quiet wait", []step{write, reply, wait(3 * time.Second), closeConn}, 1, 1},
		{"closed after a reply and a short wait", []step{write, reply, wait(time.Second), closeConn}, 0, 1},
		{"closed without writing", []step{wait(3 * time.Second), closeConn}, 0, 0},
		{"EOF before a reply", []step{write, eof, closeConn}, 1, 0},
		{"EOF after a reply", []step{write, reply, eof, closeConn}, 0, 1},
		{"EOF after a reply, then a quiet wait", []step{write, reply, eof, wait(3 * time.Second), closeConn}, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var unanswered, answered, failures atomic.Int32
				c := newScriptedConn()
				d := newDataPlaneStream(c, time.Hour, 1, dataPlaneHooks{
					onUnanswered: func(time.Time, string) { unanswered.Add(1) },
					onResponse:   func(time.Time, string) { answered.Add(1) },
					onFailure:    func(adapter.UserFailureKind) { failures.Add(1) },
				})
				d.firstResponseTimeout = 10 * time.Second
				d.unansweredAfter = 2 * time.Second
				d.reportQuiet = true
				for _, s := range tt.steps {
					s(d, c)
				}
				synctest.Wait()
				assert.Equal(t, tt.unanswered, unanswered.Load(), "unanswered reports")
				assert.Equal(t, tt.answered, answered.Load(), "responses")
				assert.Zero(t, failures.Load(), "an unanswered conn is not a window failure")
			})
		})
	}
}

func TestLocalHistory_FailureStreak(t *testing.T) {
	const window = 5 * time.Minute
	t0 := time.Now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	h := newLocalHistory()

	h.addUnanswered(streakTCP, at(0), at(-time.Second), window)
	require.EqualValues(t, 1, h.streakCount(streakTCP, at(0), window))
	h.addUnanswered(streakTCP, at(2*time.Second), at(-500*time.Millisecond), window)
	require.EqualValues(t, 1, h.streakCount(streakTCP, at(2*time.Second), window), "a conn opened before the latest failure is the same event")
	h.addUnanswered(streakTCP, at(5*time.Second), at(time.Second), window)
	require.EqualValues(t, 2, h.streakCount(streakTCP, at(5*time.Second), window), "a conn opened after the latest failure extends the streak")
	_, _, failures, _ := h.snapshot(at(5*time.Second), window)
	require.Empty(t, failures, "unanswered conns never enter the failure window")

	h.noteUserResponse(streakTCP, "", at(3*time.Second), at(5*time.Second))
	require.EqualValues(t, 2, h.streakCount(streakTCP, at(5*time.Second), window), "an answer on an older conn leaves the streak")
	h.noteUserResponse(streakTCP, "", at(6*time.Second), at(6*time.Second))
	require.Zero(t, h.streakCount(streakTCP, at(6*time.Second), window), "an answer on a newer conn ends the streak")

	require.True(t, h.addUserFailure(adapter.UserFailure{At: at(10 * time.Second)}, streakTCP, at(7*time.Second), window, 45*time.Second))
	require.False(t, h.addUserFailure(adapter.UserFailure{At: at(15 * time.Second)}, streakTCP, at(11*time.Second), window, 45*time.Second),
		"the window keeps its time dedupe")
	require.EqualValues(t, 2, h.streakCount(streakTCP, at(15*time.Second), window), "a deduped failure still extends the streak")
	require.Zero(t, h.streakCount(streakTCP, at(15*time.Second+window), window), "a streak ends once window passes without a failure")
}

func TestSelectOutbound_ReleasesStickyAfterConsecutiveFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b")
		recordSuccess(s, "a", 100)
		recordSuccess(s, "b", 110)
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()

		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		time.Sleep(time.Second)
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag(), "one unanswered conn keeps the selection")

		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		out, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "b", out.Tag(), "a second consecutive one releases it")
	})
}

func TestRecordUserFailure_StockLogger(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	factory := log.NewDefaultFactory(context.Background(), log.Formatter{}, io.Discard, "", nil, false)
	require.NoError(t, factory.Start())
	defer factory.Close()
	s.logger = factory.Logger()
	require.NotPanics(t, func() {
		s.recordUserFailure("a", adapter.UserFailureDial, s.networkGeneration.Load(), time.Time{}, "tcp")
	})
}

func TestListenPacket_ExpiredContextSkipsAlternate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		obs["a"].listen = func(ctx context.Context) (net.PacketConn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var bListens atomic.Int32
		obs["b"].listen = func(ctx context.Context) (net.PacketConn, error) {
			bListens.Add(1)
			return nil, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := s.ListenPacket(ctx, metadata.Socksaddr{})
		require.Error(t, err)
		synctest.Wait()

		assert.Zero(t, bListens.Load(), "no alternate is tried with the caller's spent context")
		failures, _ := userFailures(s, "b")
		assert.Empty(t, failures)
	})
}

// unansweredExchange waits a client's retry delay, dials through s, writes,
// waits wait and, when reply is set, reads the member's reply first, then
// closes.
func unansweredExchange(t *testing.T, s *MutableAutoSelect, members map[string]*scriptedConn, reply bool, wait time.Duration) {
	t.Helper()
	time.Sleep(250 * time.Millisecond)
	conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	member := members["a"]
	_, err = conn.Write([]byte("request"))
	require.NoError(t, err)
	if reply {
		member.reads <- scriptedRead{data: []byte("reply")}
		_, err = conn.Read(make([]byte, 8))
		require.NoError(t, err)
	}
	time.Sleep(wait)
	conn.Close()
	synctest.Wait()
}

func newStreakTestMUR(t *testing.T) (*MutableAutoSelect, map[string]*scriptedConn) {
	s, obs := newConfirmationTestMUR(t, "a", "b")
	s.hist.userFailureDedupeWindow = 0
	s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
	members := map[string]*scriptedConn{}
	obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
		// a passes probes while dropping user traffic.
		if adapter.IsProbe(ctx) {
			return probeResponderConn(), nil
		}
		members["a"] = newScriptedConn()
		return members["a"], nil
	}
	obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
	return s, members
}

func TestDialContext_UnansweredConnsReleaseSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		unansweredExchange(t, s, members, false, 3*time.Second)
		require.Equal(t, "a", s.Now(), "one unanswered conn keeps the selection")
		unansweredExchange(t, s, members, false, 3*time.Second)
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "b", out.Tag(), "a second consecutive one releases it")
	})
}

func TestDialContext_UnreachableReplacementKeepsSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		time.Sleep(10 * time.Second)
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag(), "nothing is released to a replacement whose probe fails")
		// The first selection probes a before deciding; the next decides.
		synctest.Wait()
		out, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag())
		h, _ := s.peekHistoryLocked("a")
		require.Zero(t, h.streakCount(streakTCP, time.Now(), s.hist.userFailureWindow), "with nothing reachable, the streak is not a's")
	})
}

func TestDialContext_ReleaseSkipsUnreachableReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		s.hist.userFailureDedupeWindow = 0
		s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
		members := map[string]*scriptedConn{}
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			if adapter.IsProbe(ctx) {
				return probeResponderConn(), nil
			}
			members["a"] = newScriptedConn()
			return members["a"], nil
		}
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		// Answers more slowly than b's recorded delay, so b ranks first and its
		// failed probe must be skipped.
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		time.Sleep(10 * time.Second)
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "c", out.Tag(), "the release goes to the best member whose probe passed")
	})
}

func TestDialContext_AnswerEndsStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, true, 0)
		unansweredExchange(t, s, members, false, 3*time.Second)
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag(), "an answered conn in between keeps the failures from being consecutive")
	})
}

func TestNoteUnanswered_IgnoresStaleGeneration(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	generation := s.networkGeneration.Load()
	s.InterfaceUpdated(context.Background())
	s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
	s.noteUnanswered("missing", s.networkGeneration.Load(), "tcp", "", time.Now(), time.Now())
	_, ok := s.peekHistoryLocked("missing")
	assert.False(t, ok, "a non-member gets no history")
	if h, ok := s.peekHistoryLocked("a"); ok {
		assert.Zero(t, h.streakCount(streakTCP, time.Now(), s.hist.userFailureWindow), "a report from an earlier network is dropped")
	}
}

func TestSelectOutbound_StreakReleaseCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b", "c")
		recordSuccess(s, "a", 100)
		recordSuccess(s, "b", 110)
		recordSuccess(s, "c", 120)
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()
		release := func(tag string) string {
			s.noteUnanswered(tag, generation, "tcp", "", time.Now(), time.Now())
			time.Sleep(time.Second)
			s.noteUnanswered(tag, generation, "tcp", "", time.Now(), time.Now())
			out, _, err := s.selectOutbound("tcp", "")
			require.NoError(t, err)
			return out.Tag()
		}
		require.Equal(t, "b", release("a"))

		time.Sleep(time.Second)
		s.noteUserResponse("b", generation, "tcp", "", time.Now().Add(-time.Hour), time.Now())
		time.Sleep(time.Minute)
		require.Equal(t, "b", release("b"), "a member that answered since the last release keeps the selection within the cooldown")

		time.Sleep(streakSwitchCooldown)
		require.NotEqual(t, "b", release("b"), "the cooldown passes")
	})
}

func TestSelectOutbound_StreakReleaseSkipsCooldownForSilentPick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b", "c")
		recordSuccess(s, "a", 100)
		recordSuccess(s, "b", 110)
		recordSuccess(s, "c", 120)
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()
		for _, tag := range []string{"a", "b"} {
			s.noteUnanswered(tag, generation, "tcp", "", time.Now(), time.Now())
			time.Sleep(time.Second)
			s.noteUnanswered(tag, generation, "tcp", "", time.Now(), time.Now())
			_, _, err := s.selectOutbound("tcp", "")
			require.NoError(t, err)
		}
		require.Equal(t, "c", s.Now(), "a pick that never answered is released at once")
	})
}

func TestNoteUnanswered_IgnoredWhenMemberAnsweredMeanwhile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b")
		recordSuccess(s, "a", 100)
		recordSuccess(s, "b", 110)
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()
		for range 2 {
			waiting := time.Now()
			time.Sleep(time.Second)
			// An answer on a conn opened long ago leaves the streak alone.
			s.noteUserResponse("a", generation, "tcp", "", waiting.Add(-time.Hour), time.Now())
			time.Sleep(time.Second)
			s.noteUnanswered("a", generation, "tcp", "", time.Now(), waiting)
		}
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag(), "conns that waited while the member answered others are not evidence against it")
	})
}

// A conn whose last IO was its first read must not have its report hidden by
// its own answer: the answer is stamped no later than the wait it starts.
func TestDataPlaneStream_OwnAnswerStartsTheWait(t *testing.T) {
	var answeredAt, waitingSince atomic.Value
	done := make(chan struct{})
	c := newScriptedConn()
	d := newDataPlaneStream(c, time.Hour, 1, dataPlaneHooks{
		onResponse:   func(at time.Time, _ string) { answeredAt.Store(at) },
		onUnanswered: func(since time.Time, _ string) { waitingSince.Store(since); close(done) },
	})
	d.firstResponseTimeout = 10 * time.Second
	d.unansweredAfter = 20 * time.Millisecond
	d.reportQuiet = true
	d.Write([]byte("request"))
	c.reads <- scriptedRead{data: []byte("reply")}
	d.Read(make([]byte, 8))
	time.Sleep(50 * time.Millisecond)
	d.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the quiet close was not reported")
	}
	assert.False(t, answeredAt.Load().(time.Time).After(waitingSince.Load().(time.Time)))
}

func TestSelectOutbound_SlowProbesStillRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(time.Second), nil }
		generation := s.networkGeneration.Load()
		for range 4 {
			s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
			time.Sleep(250 * time.Millisecond)
			if out, _, err := s.selectOutbound("tcp", ""); err == nil && out.Tag() == "b" {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatal("failures arriving faster than probes complete must not hold off the release")
	})
}

func TestDialContext_AwaitsPendingReplacementProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		var bDials atomic.Int32
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(context.Context) (net.Conn, error) {
			bDials.Add(1)
			return slowProbeResponderConn(600 * time.Millisecond), nil
		}
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.EqualValues(t, 2, bDials.Load(), "the dial waited for b's probe and then went to b")
	})
}

// scriptedPacketConn accepts every write and returns datagrams queued on in.
type scriptedPacketConn struct {
	net.PacketConn
	in        chan []byte
	replies   chan scriptedDatagram
	errs      chan error
	closed    chan struct{}
	closeOnce sync.Once
}

// scriptedDatagram is a datagram from a given source.
type scriptedDatagram struct {
	b    []byte
	from net.Addr
}

func newScriptedPacketConn() *scriptedPacketConn {
	return &scriptedPacketConn{
		in: make(chan []byte, 4), replies: make(chan scriptedDatagram, 4),
		errs: make(chan error, 1), closed: make(chan struct{}),
	}
}

func (c *scriptedPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (c *scriptedPacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *scriptedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case d := <-c.in:
		return copy(p, d), &net.UDPAddr{}, nil
	case d := <-c.replies:
		return copy(p, d.b), d.from, nil
	case err := <-c.errs:
		return 0, nil, err
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func TestDataPlanePacket_UnansweredWhileOpen(t *testing.T) {
	tests := []struct {
		name       string
		replyAfter time.Duration // 0: never
		closeAfter time.Duration
		// openReports is the unanswered reports before the close.
		openReports int32
		unanswered  int32
		answered    int32
	}{
		{"no reply while open", 0, 3 * time.Second, 1, 1, 0},
		{"reply in time", 500 * time.Millisecond, 3 * time.Second, 0, 0, 1},
		{"closed before the threshold", 0, time.Second, 0, 0, 0},
		{"late reply is still an answer", 2 * time.Second, 3 * time.Second, 1, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var unanswered, answered atomic.Int32
				c := newScriptedPacketConn()
				d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
					onUnanswered: func(time.Time, string) { unanswered.Add(1) },
					onResponse:   func(time.Time, string) { answered.Add(1) },
				})
				d.unansweredAfter = defaultUDPUnansweredAfter
				d.unansweredWhileOpen = true
				d.WriteTo([]byte("query"), &net.UDPAddr{})
				go d.ReadFrom(make([]byte, 64))
				if tt.replyAfter > 0 {
					time.Sleep(tt.replyAfter)
					c.in <- []byte("reply")
					time.Sleep(tt.closeAfter - tt.replyAfter)
				} else {
					time.Sleep(tt.closeAfter)
				}
				synctest.Wait()
				assert.Equal(t, tt.openReports, unanswered.Load(), "unanswered reports while open")
				d.Close()
				synctest.Wait()
				assert.Equal(t, tt.unanswered, unanswered.Load(), "unanswered reports")
				assert.Equal(t, tt.answered, answered.Load(), "answers")
			})
		})
	}
}

func TestNoteUnanswered_UDPNeedsAnAnsweredDestination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b")
		generation := s.networkGeneration.Load()
		twice := func(dest string) uint32 {
			s.noteUnanswered("a", generation, "udp", dest, time.Now(), time.Now())
			time.Sleep(time.Second)
			s.noteUnanswered("a", generation, "udp", dest, time.Now(), time.Now())
			h, _ := s.peekHistoryLocked("a")
			return h.streakCount(streakUDP, time.Now(), s.hist.userFailureWindow)
		}
		require.Zero(t, twice("silent.test:123"), "a destination that never answered is no evidence")

		s.noteUserResponse("a", generation, "udp", "service.test:443", time.Now(), time.Now())
		time.Sleep(time.Second)
		require.EqualValues(t, 2, twice("service.test:443"), "a destination that answered before is")
	})
}

func TestFailureStreak_PerNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b")
		recordSuccess(s, "a", 100)
		recordSuccess(s, "b", 110)
		s.stickyTag.tcp.Store("a")
		s.stickyTag.udp.Store("a")
		generation := s.networkGeneration.Load()
		s.noteUserResponse("a", generation, "udp", "dns:53", time.Now(), time.Now())
		time.Sleep(time.Second)
		for range 2 {
			s.noteUnanswered("a", generation, "udp", "dns:53", time.Now(), time.Now())
			time.Sleep(time.Second)
			// A TCP answer on a newer conn doesn't end the UDP streak.
			s.noteUserResponse("a", generation, "tcp", "", time.Now(), time.Now())
			time.Sleep(time.Second)
		}
		udp, _, err := s.selectOutbound("udp", "")
		require.NoError(t, err)
		tcp, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "b", udp.Tag(), "UDP failing twice in a row releases the UDP selection")
		assert.Equal(t, "a", tcp.Tag(), "and leaves the TCP selection alone")
	})
}

func TestNoteUserResponse_AnyUDPAnswerEndsStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a")
		generation := s.networkGeneration.Load()
		old := time.Now()
		s.noteUserResponse("a", generation, "udp", "dns:53", old, old)
		time.Sleep(time.Second)
		s.noteUnanswered("a", generation, "udp", "dns:53", time.Now(), time.Now())
		time.Sleep(time.Second)
		s.noteUserResponse("a", generation, "udp", "dns:53", old, time.Now())
		h, _ := s.peekHistoryLocked("a")
		assert.Zero(t, h.streakCount(streakUDP, time.Now(), s.hist.userFailureWindow),
			"a late reply on a handle opened before the failure still shows UDP working")
	})
}

func TestListenPacket_UnansweredHandlesReleaseSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		obs["a"].listen = func(context.Context) (net.PacketConn, error) { return newScriptedPacketConn(), nil }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		dest := metadata.ParseSocksaddrHostPort("dns.test", 53)
		exchange := func(reply bool) {
			time.Sleep(250 * time.Millisecond)
			pc, err := s.ListenPacket(context.Background(), dest)
			require.NoError(t, err)
			pc.WriteTo([]byte("query"), &net.UDPAddr{})
			if reply {
				inner := pc.(*adapter.TaggedPacketConn).PacketConn.(*dataPlanePacket).PacketConn.(*scriptedPacketConn)
				inner.in <- []byte("reply")
				pc.ReadFrom(make([]byte, 64))
			}
			time.Sleep(2 * time.Second)
			pc.Close()
			synctest.Wait()
		}
		exchange(true)
		exchange(false)
		require.Equal(t, "a", loadString(&s.stickyTag.udp), "one unanswered handle keeps the selection")
		exchange(false)
		out, _, err := s.selectOutbound("udp", "")
		require.NoError(t, err)
		require.Equal(t, "b", out.Tag(), "a second consecutive one releases it")
	})
}

func TestNoteUserResponse_KeepsStoredHistory(t *testing.T) {
	for _, note := range []string{"response", "unanswered"} {
		t.Run(note, func(t *testing.T) {
			s, _ := newTestMUR(t, "a", "b")
			s.history.Store("a", &adapter.TagHistory{LastSuccessDelayMs: 100, UpdatedAt: time.Now()})
			recordSuccess(s, "b", 105)
			s.stickyTag.tcp.Store("a")
			out, _, err := s.selectOutbound("tcp", "")
			require.NoError(t, err)
			require.Equal(t, "a", out.Tag())
			generation := s.networkGeneration.Load()
			if note == "response" {
				s.noteUserResponse("a", generation, "tcp", "", time.Now(), time.Now())
			} else {
				s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
			}
			out, _, err = s.selectOutbound("tcp", "")
			require.NoError(t, err)
			require.Equal(t, "a", out.Tag(), "noting a stored member's traffic keeps its stored delay")
		})
	}
}

func TestNoteUnanswered_UDPDestinationOutlivesFailureWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a")
		generation := s.networkGeneration.Load()
		s.noteUserResponse("a", generation, "udp", "dns:53", time.Now(), time.Now())
		time.Sleep(s.hist.userFailureWindow + time.Minute)
		for range 2 {
			s.noteUnanswered("a", generation, "udp", "dns:53", time.Now(), time.Now())
			time.Sleep(time.Second)
		}
		h, _ := s.peekHistoryLocked("a")
		require.EqualValues(t, 2, h.streakCount(streakUDP, time.Now(), s.hist.userFailureWindow),
			"a relay that dies after a quiet spell is still detectable")
	})
}

func TestDataPlanePacket_OpenReportKeepsResetDetection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unanswered, failures atomic.Int32
		c := newScriptedPacketConn()
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onUnanswered: func(time.Time, string) { unanswered.Add(1) },
			onFailure:    func(adapter.UserFailureKind) { failures.Add(1) },
		})
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		d.WriteTo([]byte("query"), &net.UDPAddr{})
		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, unanswered.Load())
		c.errs <- connResetErr()
		d.ReadFrom(make([]byte, 64))
		synctest.Wait()
		require.EqualValues(t, 1, failures.Load(), "a reset after an unanswered report is still charged")
		d.Close()
	})
}

func TestDataPlanePacket_EachExchangeWatched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type report struct {
			since time.Time
			dest  string
		}
		var mu sync.Mutex
		var reports []report
		var answers []string
		c := newScriptedPacketConn()
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onUnanswered: func(since time.Time, dest string) {
				mu.Lock()
				reports = append(reports, report{since, dest})
				mu.Unlock()
			},
			onResponse: func(_ time.Time, dest string) {
				mu.Lock()
				answers = append(answers, dest)
				mu.Unlock()
			},
		})
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		go func() {
			for {
				if _, _, err := d.ReadFrom(make([]byte, 64)); err != nil {
					return
				}
			}
		}()
		first := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 53}
		second := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 53}

		d.WriteTo([]byte("q1"), first)
		c.in <- []byte("a1")
		time.Sleep(100 * time.Millisecond)
		c.in <- []byte("a1 again")
		time.Sleep(2 * time.Second)

		sent := time.Now()
		d.WriteTo([]byte("q2"), second)
		time.Sleep(2 * time.Second)
		d.WriteTo([]byte("q2 retry"), second)
		time.Sleep(2 * time.Second)
		c.in <- []byte("late a2")
		synctest.Wait()

		mu.Lock()
		defer mu.Unlock()
		require.Len(t, reports, 2, "an answered handle's later exchanges are each watched")
		assert.Equal(t, sent, reports[0].since)
		assert.Equal(t, second.String(), reports[0].dest, "a report names the datagram's own destination")
		assert.True(t, reports[1].since.After(reports[0].since), "a send after a report starts a new wait")
		assert.Equal(t, []string{first.String(), second.String()}, answers,
			"replies are passed on at most once a second, and at once after a report")
	})
}

// listenScripted makes members' ListenPacket return scripted handles, keeping
// each in handles.
func listenScripted(obs map[string]*mockOutbound, handles map[string][]*scriptedPacketConn, tags ...string) {
	for _, tag := range tags {
		obs[tag].listen = func(context.Context) (net.PacketConn, error) {
			c := newScriptedPacketConn()
			handles[tag] = append(handles[tag], c)
			return c, nil
		}
	}
}

func isClosed(c *scriptedPacketConn) bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func TestCloseStuckUDP_ClosesOnlyUnansweredHandles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		handles := map[string][]*scriptedPacketConn{}
		listenScripted(obs, handles, "a")
		dest := metadata.ParseSocksaddrHostPort("dns.test", 53)
		answering, err := s.ListenPacket(context.Background(), dest)
		require.NoError(t, err)
		stuck, err := s.ListenPacket(context.Background(), dest)
		require.NoError(t, err)
		go answering.ReadFrom(make([]byte, 64))
		answering.WriteTo([]byte("q"), &net.UDPAddr{})
		handles["a"][0].in <- []byte("a")
		stuck.WriteTo([]byte("q"), &net.UDPAddr{})
		time.Sleep(2 * time.Second)

		s.access.Lock()
		s.closeStuckUDPLocked("a")
		s.access.Unlock()
		synctest.Wait()
		assert.False(t, isClosed(handles["a"][0]), "a handle still getting replies keeps its session")
		assert.True(t, isClosed(handles["a"][1]), "a handle whose exchange went unanswered is closed")
		answering.Close()
	})
}

func TestSelectOutbound_DelayReleaseClosesNoSessions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		handles := map[string][]*scriptedPacketConn{}
		listenScripted(obs, handles, "a")
		pc, err := s.ListenPacket(context.Background(), metadata.ParseSocksaddrHostPort("dns.test", 53))
		require.NoError(t, err)
		pc.WriteTo([]byte("q"), &net.UDPAddr{})
		time.Sleep(2 * time.Second)
		recordSuccess(s, "a", 500)
		recordSuccess(s, "b", 100)
		out, _, err := s.selectOutbound("udp", "")
		require.NoError(t, err)
		require.Equal(t, "b", out.Tag(), "b wins on delay")
		synctest.Wait()
		assert.False(t, isClosed(handles["a"][0]), "a switch between working members moves no session")
		pc.Close()
	})
}

func TestListenPacket_SessionMovedOffFailingMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		handles := map[string][]*scriptedPacketConn{}
		listenScripted(obs, handles, "a")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		pc, err := s.ListenPacket(context.Background(), metadata.ParseSocksaddrHostPort("dns.test", 53))
		require.NoError(t, err)
		go func() {
			for {
				if _, _, err := pc.ReadFrom(make([]byte, 64)); err != nil {
					return
				}
			}
		}()
		pc.WriteTo([]byte("q"), &net.UDPAddr{})
		handles["a"][0].in <- []byte("a")
		time.Sleep(time.Second)
		for range 2 {
			pc.WriteTo([]byte("q"), &net.UDPAddr{})
			time.Sleep(2250 * time.Millisecond)
		}
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.stickyTag.udp), "the UDP selection moves without another ListenPacket")
		assert.True(t, isClosed(handles["a"][0]), "and the stuck session is closed so it is rebuilt through b")
	})
}

func TestFailureStreak_UDPRetryWindow(t *testing.T) {
	const window = 5 * time.Minute
	report := defaultUDPUnansweredAfter
	// Exchanges start every period and are reported report later.
	streak := func(period time.Duration) uint32 {
		t0 := time.Now()
		h := newLocalHistory()
		h.addUnanswered(streakUDP, t0.Add(report), t0, window)
		h.addUnanswered(streakUDP, t0.Add(period+report), t0.Add(period), window)
		return h.streakCount(streakUDP, t0.Add(period+report), window)
	}
	require.EqualValues(t, 1, streak(25*time.Second), "one-way keepalives don't add up")
	require.EqualValues(t, 1, streak(10*time.Second), "even at a 10s period")
	require.EqualValues(t, 2, streak(5*time.Second), "a resolver's 5s retry does")
	require.EqualValues(t, 2, streak(2250*time.Millisecond), "and a prompt retry")
	t0 := time.Now()
	h := newLocalHistory()
	h.addUnanswered(streakTCP, t0, t0, window)
	h.addUnanswered(streakTCP, t0.Add(time.Minute), t0.Add(59*time.Second), window)
	require.EqualValues(t, 2, h.streakCount(streakTCP, t0.Add(time.Minute), window), "TCP has no retry window")
}

func TestDataPlanePacket_CreditsReplySource(t *testing.T) {
	var got atomic.Value
	c := newScriptedPacketConn()
	d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
		onResponse: func(_ time.Time, dest string) { got.Store(dest) },
	})
	d.unansweredAfter = defaultUDPUnansweredAfter
	d.unansweredWhileOpen = true
	defer d.Close()
	d.WriteTo([]byte("q"), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 6881})
	c.replies <- scriptedDatagram{b: []byte("r"), from: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 9), Port: 6881}}
	d.ReadFrom(make([]byte, 64))
	assert.Equal(t, "192.0.2.9:6881", got.Load(), "a reply is credited to its own source, not the peer waited on")
}

func TestListenPacket_OneWayKeepaliveKeepsSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		handles := map[string][]*scriptedPacketConn{}
		listenScripted(obs, handles, "a")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		peer := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 3), Port: 51820}
		pc, err := s.ListenPacket(context.Background(), metadata.ParseSocksaddrHostPort("wg.test", 51820))
		require.NoError(t, err)
		go func() {
			for {
				if _, _, err := pc.ReadFrom(make([]byte, 64)); err != nil {
					return
				}
			}
		}()
		pc.WriteTo([]byte("handshake"), peer)
		handles["a"][0].replies <- scriptedDatagram{b: []byte("response"), from: peer}
		for range 8 {
			time.Sleep(25 * time.Second)
			pc.WriteTo([]byte("keepalive"), peer)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.Equal(t, "a", loadString(&s.stickyTag.udp), "unanswered keepalives don't release a working member")
		assert.False(t, isClosed(handles["a"][0]), "or close its session")
		pc.Close()
	})
}

// hangUntilDone is a dial that never connects before its context ends.
func hangUntilDone(ctx context.Context) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// failingProbes makes tag's probes fail and counts them in n, leaving other
// dials to user.
func failingProbes(ob *mockOutbound, n *atomic.Int32, user func(context.Context) (net.Conn, error)) {
	ob.dial = func(ctx context.Context) (net.Conn, error) {
		if adapter.IsProbe(ctx) {
			n.Add(1)
			return nil, errors.New("probe failed")
		}
		return user(ctx)
	}
}

func TestDialContext_RescuesHangingDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Less(t, time.Since(start), 5*time.Second, "the dial is rescued long before the caller's deadline")
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp))
		assert.EqualValues(t, 2, aProbes.Load(), "two failed confirmations")
		synctest.Wait()
	})
}

func TestConfirmation_OneFailureDoesNotSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var probes atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if probes.Add(1) == 1 {
				return nil, errors.New("chance failure")
			}
			return probeResponderConn(), nil
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		assert.EqualValues(t, 2, probes.Load(), "a failed confirmation is confirmed once more")
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp), "one failed probe doesn't move the selection")
	})
}

func TestEarlyConfirmation_Cooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var probes atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			probes.Add(1)
			return probeResponderConn(), nil
		}
		generation := s.networkGeneration.Load()
		s.requestEarlyConfirmation("a", generation)
		synctest.Wait()
		time.Sleep(10 * time.Second)
		s.requestEarlyConfirmation("a", generation)
		synctest.Wait()
		require.EqualValues(t, 1, probes.Load(), "no second early confirmation within the cooldown")
		time.Sleep(earlyConfirmCooldown)
		s.requestEarlyConfirmation("a", generation)
		synctest.Wait()
		require.EqualValues(t, 2, probes.Load(), "another once it passes")
	})
}

func TestConfirmation_OutageHoldsOffConfirmations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes, bProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		failingProbes(obs["b"], &bProbes, hangUntilDone)
		generation := s.networkGeneration.Load()
		s.requestFailureConfirmation("a", "", generation)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.stickyTag.tcp), "nothing to move to")
		probed := aProbes.Load()
		s.requestFailureConfirmation("a", "", generation)
		synctest.Wait()
		assert.Equal(t, probed, aProbes.Load(), "with nothing reachable, confirmations are held off")

		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		requestTestProbes(s, []string{"b"}, probeForced)
		synctest.Wait()
		s.requestFailureConfirmation("a", "", generation)
		synctest.Wait()
		assert.Greater(t, aProbes.Load(), probed, "a passing probe ends the hold-off")
	})
}

func TestConfirmation_SwitchWaitsForReplacementProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(time.Second)
			return probeResponderConn(), nil
		}
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp),
			"a's second failure before b's probe completes still switches once it passes")
	})
}

func TestConfirmation_ClosesDoomedConns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
		user := newScriptedConn()
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, func(context.Context) (net.Conn, error) { return user, nil })
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Write([]byte("request"))
		closed := make(chan struct{})
		go func() {
			conn.Read(make([]byte, 8))
			close(closed)
		}()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp))
		select {
		case <-closed:
		default:
			t.Fatal("a conn awaiting its first response from a failed member is closed")
		}
	})
}

func TestEarlyConfirmation_SkippedRearmLeavesNoStrike(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		release := make(chan struct{})
		var probes atomic.Int32
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			if probes.Add(1) == 1 {
				<-release
				return nil, errors.New("probe failed")
			}
			return probeResponderConn(), nil
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		generation := s.networkGeneration.Load()
		s.requestEarlyConfirmation("a", generation)
		synctest.Wait()
		s.stickyTag.tcp.Store("b")
		close(release)
		synctest.Wait()
		s.stickyTag.tcp.Store("a")
		time.Sleep(2 * time.Minute)
		s.requestEarlyConfirmation("a", generation)
		synctest.Wait()
		assert.EqualValues(t, 2, probes.Load(), "a first strike whose second confirmation never ran doesn't hold off early confirmation")
	})
}

func TestProbeTCPReplacements_NoneToProbeRequestsNothing(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.access.Lock()
	defer s.access.Unlock()
	require.Zero(t, s.probeTCPReplacementsLocked("a", time.Now()))
	assert.Equal(t, probeNotNeeded, s.probeMembers["a"].need, "with no replacement to probe, the selection isn't probed either")
}

func TestRank_LatestProbeFailedRanksLower(t *testing.T) {
	s, _ := newTestMUR(t, "fast-failing", "slow-passing")
	recordSuccess(s, "fast-failing", 20)
	recordSuccess(s, "slow-passing", 80)
	s.historyForLocked("fast-failing").recordProbeFailure(time.Now())
	out, _, err := s.selectOutbound("tcp", "")
	require.NoError(t, err)
	assert.Equal(t, "slow-passing", out.Tag(), "a member whose latest probe failed ranks after one whose latest passed")
}

func TestConfirmation_ProbesPastFailedReplacements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tags := []string{"a", "b", "c", "d", "e"}
		s, obs := newConfirmationTestMUR(t, tags...)
		var probes atomic.Int32
		for _, tag := range tags[:4] {
			failingProbes(obs[tag], &probes, hangUntilDone)
		}
		obs["e"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		assert.Equal(t, "e", loadString(&s.stickyTag.tcp),
			"when the first replacements probed fail too, the next are probed before giving up")
	})
}

func TestRank_UnmeasuredStaysBehindFailedProbe(t *testing.T) {
	s, _ := newTestMUR(t, "a", "c")
	recordSuccess(s, "a", 100)
	s.historyForLocked("a").recordProbeFailure(time.Now())
	s.stickyTag.tcp.Store("a")
	out, _, err := s.selectOutbound("tcp", "")
	require.NoError(t, err)
	assert.Equal(t, "a", out.Tag(), "one failed probe doesn't hand the selection to an unmeasured member")
}

// networkDownUntil is a dial over a network that silently drops everything
// until up, then works, like a brief outage of the device's own network.
func networkDownUntil(up time.Time) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		if wait := time.Until(up); wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		return probeResponderConn(), nil
	}
}

func TestConfirmation_RecheckAfterMixedPicture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tags := []string{"a", "b", "c", "d", "e"}
		s, obs := newConfirmationTestMUR(t, tags...)
		// a's two confirmations and b-d's probes fail; e, probed next,
		// passes once the network is back.
		up := time.Now().Add(6500 * time.Millisecond)
		for _, tag := range tags {
			obs[tag].dial = networkDownUntil(up)
		}
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(time.Minute)
		synctest.Wait()
		s.access.Lock()
		e, a := s.probeMembers["e"].outcome, s.probeMembers["a"].outcome
		s.access.Unlock()
		require.NotNil(t, e)
		require.NotNil(t, a)
		require.True(t, e.success, "e, probed after b-d failed, passes")
		require.True(t, a.success, "a is rechecked once e passes")
		require.False(t, a.completed.Before(e.completed), "a's recheck follows e's pass")
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp),
			"when replacements failed too and a's recheck passes, the failures were the network's")
	})
}

func TestOutage_SuspicionLastsUntilAnAnswer(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "x")
	recordSuccess(s, "a", 100)
	recordSuccess(s, "x", 40)
	s.access.Lock()
	s.historyForLocked("x").recordProbeFailure(time.Now().Add(-time.Hour))
	evidence := time.Now()
	s.historyForLocked("a").recordProbeFailure(evidence)
	s.suspectOutageLocked(evidence)
	s.access.Unlock()
	h, _ := s.peekHistoryLocked("a")
	_, consec, _, _ := h.snapshot(time.Now(), time.Hour)
	assert.Zero(t, consec, "probe failures that led to the verdict are forgiven")
	x, _ := s.peekHistoryLocked("x")
	_, consec, _, _ = x.snapshot(time.Now(), time.Hour)
	assert.EqualValues(t, 1, consec, "a member failing before the verdict stays failed")
	s.outageSince = s.outageSince.Add(-time.Hour)
	s.access.Lock()
	assert.True(t, s.outageSuspectedLocked(), "the suspicion has no time limit")
	assert.True(t, s.outageEvidenceLocked(time.Now()), "failures during it build no streak")
	s.access.Unlock()

	opened := time.Now().Add(-time.Second)
	s.noteUserResponse("b", s.networkGeneration.Load(), "tcp", "", time.Now(), time.Now())
	s.access.Lock()
	defer s.access.Unlock()
	assert.False(t, s.outageSuspectedLocked(), "an answer ends it")
	assert.True(t, s.outageEvidenceLocked(opened), "a conn opened during it still says nothing about its member")
	assert.False(t, s.outageEvidenceLocked(time.Now().Add(time.Second)), "one opened after it does")
}

func TestStreak_TCPSelectionFailingProbesLeftToConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b")
		recordSuccess(s, "a", 100)
		recordSuccess(s, "b", 110)
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()
		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		time.Sleep(time.Second)
		s.access.Lock()
		s.recordProbeOutcomeLocked("a", &probeOutcome{success: false, generation: generation, url: s.probeURLForLocked("a")})
		s.probeMembers["a"].firstConfirmFailedAt = time.Now()
		s.access.Unlock()
		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", out.Tag(), "a selection whose own probe failed during the streak is the confirmations' to judge")
	})
}

func TestConfirmation_ClosesFreshUnansweredConns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, func(context.Context) (net.Conn, error) { return newScriptedConn(), nil })
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Write([]byte("request"))
		closed := make(chan struct{})
		go func() {
			conn.Read(make([]byte, 8))
			close(closed)
		}()
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		require.Equal(t, "b", loadString(&s.stickyTag.tcp))
		select {
		case <-closed:
		default:
			t.Fatal("a conn its failed member hasn't answered yet is closed, however new")
		}
	})
}

func TestOutage_ForgivesOnlyFailuresSinceEvidence(t *testing.T) {
	h := newLocalHistory()
	t0 := time.Now()
	h.recordProbeSuccess(100, t0)
	h.recordProbeFailure(t0.Add(time.Minute))
	evidence := t0.Add(3 * time.Minute)
	h.recordProbeFailure(evidence)
	h.recordProbeFailure(evidence.Add(3 * time.Second))
	h.forgiveProbeFailures(evidence)
	_, consec, _, _ := h.snapshot(evidence.Add(time.Minute), time.Hour)
	assert.EqualValues(t, 1, consec, "an earlier chance failure stays counted, the outage's don't")
}

func TestDataPlanePacket_CreditsEachNewSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got []string
		c := newScriptedPacketConn()
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onResponse: func(_ time.Time, dest string) { got = append(got, dest) },
		})
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		a := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 53}
		b := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 53}
		d.WriteTo([]byte("q"), a)
		d.WriteTo([]byte("q"), b)
		for _, from := range []net.Addr{a, b, a} {
			c.replies <- scriptedDatagram{b: []byte("r"), from: from}
			d.ReadFrom(make([]byte, 64))
		}
		assert.Equal(t, []string{"192.0.2.1:53", "192.0.2.2:53"}, got,
			"each source's first reply is passed on, a repeat within a second isn't")
	})
}

// sendHookPacketConn runs onSend inside WriteTo, before it returns err or
// success.
type sendHookPacketConn struct {
	*scriptedPacketConn
	onSend func()
	err    error
}

func (c *sendHookPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if c.onSend != nil {
		c.onSend()
	}
	if c.err != nil {
		return 0, c.err
	}
	return len(p), nil
}

func TestDataPlanePacket_WaitStartsBeforeTheSend(t *testing.T) {
	tests := []struct {
		name  string
		reply bool
		err   error
	}{
		{"a reply read before the send returns ends the wait", true, nil},
		{"a failed send leaves no wait", false, errors.New("send failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var unanswered atomic.Int32
				c := &sendHookPacketConn{scriptedPacketConn: newScriptedPacketConn(), err: tt.err}
				d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
					onUnanswered: func(time.Time, string) { unanswered.Add(1) },
				})
				d.unansweredAfter = defaultUDPUnansweredAfter
				d.unansweredWhileOpen = true
				defer d.Close()
				if tt.reply {
					c.onSend = func() {
						c.in <- []byte("reply")
						d.ReadFrom(make([]byte, 64))
					}
				}
				d.WriteTo([]byte("query"), &net.UDPAddr{})
				time.Sleep(2 * defaultUDPUnansweredAfter)
				synctest.Wait()
				assert.Zero(t, unanswered.Load(), "no unanswered report")
				assert.False(t, d.awaitingReply(time.Now()), "nothing awaits a reply")
			})
		})
	}
}

func TestDataPlanePacket_StallKeepsExchangesWatched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unanswered, stalls atomic.Int32
		c := newScriptedPacketConn()
		d := newDataPlanePacket(c, time.Second, 1, dataPlaneHooks{
			onUnanswered: func(time.Time, string) { unanswered.Add(1) },
			onFailure:    func(adapter.UserFailureKind) { stalls.Add(1) },
		})
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		d.WriteTo([]byte("q"), &net.UDPAddr{})
		c.in <- []byte("r")
		d.ReadFrom(make([]byte, 64))
		d.WriteTo([]byte("q"), &net.UDPAddr{})
		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, stalls.Load(), "the handle stalled")
		assert.EqualValues(t, 1, unanswered.Load(), "its unanswered exchange is still reported")
	})
}

func TestDataPlaneStream_AnsweredBeforeHooksFinishIsNotDoomed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := newScriptedConn()
		d := newDataPlaneStream(c, time.Hour, 1<<20, dataPlaneHooks{
			onResponse: func(time.Time, string) { <-release },
		})
		d.firstResponseTimeout = defaultFirstResponseTimeout
		d.unansweredAfter = defaultUnansweredAfter
		defer d.Close()
		d.Write([]byte("request"))
		time.Sleep(time.Second)
		c.reads <- scriptedRead{data: []byte("reply")}
		go d.Read(make([]byte, 8))
		synctest.Wait()
		assert.False(t, d.awaitingFirstResponse(time.Now(), 0), "data that arrived counts while its read's hooks run")
		close(release)
	})
}

// blockingWriteConn's Write blocks until release is closed, as a write
// carrying a deferred handshake to an unresponsive server does.
type blockingWriteConn struct {
	*scriptedConn
	release chan struct{}
}

func (c *blockingWriteConn) Write(p []byte) (int, error) {
	<-c.release
	return len(p), nil
}

func TestDataPlaneStream_BlockedFirstWriteAwaitsResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var slow atomic.Int32
		c := &blockingWriteConn{scriptedConn: newScriptedConn(), release: make(chan struct{})}
		d := newDataPlaneStream(c, time.Hour, 1<<20, dataPlaneHooks{
			onSlowResponse: func() { slow.Add(1) },
		})
		d.firstResponseTimeout = defaultFirstResponseTimeout
		d.unansweredAfter = defaultUnansweredAfter
		d.slowResponseAfter = earlyConfirmAfter
		go d.Write([]byte("request"))
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assert.True(t, d.awaitingFirstResponse(time.Now(), earlyConfirmAfter), "a conn blocked in its first write is waiting")
		assert.EqualValues(t, 1, slow.Load(), "and asks for an early confirmation")
		close(c.release)
		d.Close()
	})
}

func TestDialContext_DeadlineDuringReplacementWaitDialsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(5 * time.Second), nil }
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		last := members["a"]
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Same(t, last, members["a"], "no member is dialed with the caller's spent context")
		time.Sleep(10 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_AlternateCutOffByDeadlineIsNotCharged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("refused") }
		obs["b"].dial = hangUntilDone
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.Error(t, err)
		synctest.Wait()
		failures, _ := userFailures(s, "b")
		assert.Empty(t, failures, "the alternate is not charged for the caller's deadline")
	})
}

func TestConfirmation_ProbesSoftDemotedReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.access.Lock()
		h := s.historyForLocked("b")
		for i := range s.hist.softFailLimit {
			h.addWindowFailure(adapter.UserFailure{At: time.Now().Add(time.Duration(i) * time.Second), Kind: adapter.UserFailureDial}, s.hist.userFailureWindow, 0)
		}
		s.access.Unlock()
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp), "a soft-demoted member is probed and taken when the selection fails")
		s.access.Lock()
		defer s.access.Unlock()
		assert.False(t, s.outageSuspectedLocked(), "with a replacement passing, there's no outage")
	})
}

func TestConfirmation_CooldownVetoIsNoOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.access.Lock()
		s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = time.Now(), "x"
		s.access.Unlock()
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		require.EqualValues(t, 2, aProbes.Load(), "both confirmations failed")
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp), "the switch cooldown holds the selection")
		s.access.Lock()
		defer s.access.Unlock()
		assert.False(t, s.outageSuspectedLocked(), "a cooldown veto with a replacement passing is no outage")
	})
}

func TestConfirmation_LongProbeTimeoutStillConcludes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		obs["a"].typeName = lConst.TypeUnbounded
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			aProbes.Add(1)
			return hangUntilDone(ctx)
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp),
			"confirmations that each take longer than the strike window still switch")
	})
}

func TestAdvanceNetwork_ClearsConfirmationStrikes(t *testing.T) {
	s, _ := newConfirmationTestMUR(t, "a", "b")
	s.access.Lock()
	defer s.access.Unlock()
	s.probeMembers["a"].firstConfirmFailedAt = time.Now()
	s.probeMembers["a"].confirmedAt = time.Now()
	s.pendingConfirmedFailure.tag, s.pendingConfirmedFailure.at = "a", time.Now()
	s.advanceNetworkLocked()
	assert.True(t, s.probeMembers["a"].firstConfirmFailedAt.IsZero(), "an old network's strike is dropped")
	assert.True(t, s.probeMembers["a"].confirmedAt.IsZero(), "as is its early-confirmation cooldown")
	assert.Empty(t, s.pendingConfirmedFailure.tag, "and its pending switch")
}

func TestStreak_FailedProbeWithoutConfirmationReleases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		generation := s.networkGeneration.Load()
		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		time.Sleep(time.Second)
		s.access.Lock()
		s.recordProbeOutcomeLocked("a", &probeOutcome{success: false, generation: generation, url: s.probeURLForLocked("a")})
		s.access.Unlock()
		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		synctest.Wait()
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "b", out.Tag(), "with no confirmation under way, a failed probe doesn't hold off the streak's release")
	})
}

func TestStreak_PassingSelectionIsNoOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(context.Context) (net.Conn, error) { return nil, errors.New("probe failed") }
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag(), "with no replacement reachable, the selection stays")
		s.access.Lock()
		defer s.access.Unlock()
		assert.False(t, s.outageSuspectedLocked(), "its own passing probe shows the device's network works")
	})
}

func TestSelectOutbound_StreakReleaseToUnmeasuredWaitsForProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newTestMUR(t, "a", "c")
		s.defaultURL = "http://probe.test/"
		recordSuccess(s, "a", 100)
		obs["c"].dial = hangUntilDone
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()
		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		time.Sleep(time.Second)
		s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", out.Tag(), "a streak-demoted selection waits for an unmeasured member's probe to pass")
	})
}

func TestOutage_ForgivenessIsPersisted(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	t0 := time.Now()
	s.access.Lock()
	defer s.access.Unlock()
	s.mutateHistoryLocked("b", func(h *localHistory, now time.Time) bool {
		h.recordProbeFailure(now)
		h.recordProbeFailure(now)
		return true
	})
	require.EqualValues(t, 2, s.history.Load("b").ConsecutiveFailures)
	s.suspectOutageLocked(t0)
	assert.Zero(t, s.history.Load("b").ConsecutiveFailures, "the stored history drops the forgiven failures")
}

func TestOutage_SuspicionDropsFailureStreaks(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	recordSuccess(s, "a", 100)
	s.stickyTag.tcp.Store("a")
	generation := s.networkGeneration.Load()
	s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
	s.noteUnanswered("a", generation, "tcp", "", time.Now().Add(time.Second), time.Now().Add(time.Second))
	s.access.Lock()
	defer s.access.Unlock()
	h := s.historyForLocked("a")
	require.EqualValues(t, 2, h.streakCount(streakTCP, time.Now().Add(time.Second), s.hist.userFailureWindow))
	s.suspectOutageLocked(time.Now())
	assert.Zero(t, h.streakCount(streakTCP, time.Now().Add(time.Second), s.hist.userFailureWindow),
		"failures that led to a suspected outage don't release the selection once it ends")
}

func TestDialContext_AbandonedDialIsCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		var abandoned atomic.Bool
		failingProbes(obs["a"], &aProbes, func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			abandoned.Store(true)
			return nil, ctx.Err()
		})
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		defer conn.Close()
		synctest.Wait()
		assert.True(t, abandoned.Load(), "the abandoned dial ends with the failover, not at the caller's deadline")
	})
}

// joinedSendPacketConn fails its first send after running a second, nested
// one that succeeds, as a concurrent writer's would.
type joinedSendPacketConn struct {
	*scriptedPacketConn
	d     *dataPlanePacket
	sends int
}

func (c *joinedSendPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.sends++
	if c.sends == 1 {
		c.d.WriteTo(p, addr)
		return 0, errors.New("send failed")
	}
	return len(p), nil
}

func TestDataPlanePacket_FailedSendKeepsJoinedWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unanswered atomic.Int32
		c := &joinedSendPacketConn{scriptedPacketConn: newScriptedPacketConn()}
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onUnanswered: func(time.Time, string) { unanswered.Add(1) },
		})
		c.d = d
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		d.WriteTo([]byte("query"), &net.UDPAddr{})
		time.Sleep(2 * defaultUDPUnansweredAfter)
		synctest.Wait()
		assert.EqualValues(t, 1, unanswered.Load(), "the send that succeeded is still watched")
	})
}

func TestDialContext_DialContextLivesUntilClose(t *testing.T) {
	s, obs := newConfirmationTestMUR(t, "a", "b")
	var dialCtx context.Context
	obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
		dialCtx = ctx
		return newScriptedConn(), nil
	}
	conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	require.NoError(t, dialCtx.Err(), "a conn tied to its dial context stays usable while open")
	conn.Close()
	assert.ErrorIs(t, dialCtx.Err(), context.Canceled, "and the context is released once it closes")
}

func TestStreak_StaleSelectionProbeIsNoOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(context.Context) (net.Conn, error) { return nil, errors.New("probe failed") }
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		s.access.Lock()
		stale := s.probeMembers["a"].outcome.sequence
		s.access.Unlock()
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		// The first selection probes a, whose earlier pass is stale; the
		// next decides on that fresh pass.
		synctest.Wait()
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", out.Tag(), "with no replacement reachable, the selection stays")
		s.access.Lock()
		defer s.access.Unlock()
		require.Greater(t, s.probeMembers["a"].outcome.sequence, stale, "a was probed afresh")
		require.True(t, s.probeMembers["a"].outcome.success)
		assert.False(t, s.outageSuspectedLocked(), "its fresh pass shows the device's network works")
	})
}

func TestDataPlanePacket_CreditsNewSourcePastTheLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got []string
		c := newScriptedPacketConn()
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onResponse: func(_ time.Time, dest string) { got = append(got, dest) },
		})
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		reply := func(i int) {
			c.replies <- scriptedDatagram{b: []byte("r"), from: &net.UDPAddr{IP: net.IPv4(192, 0, 2, byte(i)), Port: 53}}
			d.ReadFrom(make([]byte, 64))
		}
		for i := range maxUDPAnsweredDestinations {
			reply(i + 1)
		}
		require.Len(t, got, maxUDPAnsweredDestinations, "each of the first sources is passed on at once")
		reply(65)
		require.Len(t, got, maxUDPAnsweredDestinations, "past the limit, a new source waits for the once-a-second pass")
		time.Sleep(time.Second)
		reply(65)
		reply(66)
		assert.Equal(t, []string{"192.0.2.65:53"}, got[maxUDPAnsweredDestinations:], "then it is passed on, one source a second")
	})
}

// expiredUnseenCtx reports its deadline through Err from at on, before its
// Done channel would close, as a context whose deadline passed at the same
// instant as another timer may.
type expiredUnseenCtx struct {
	context.Context
	at time.Time
}

func (c expiredUnseenCtx) Done() <-chan struct{} { return nil }

func (c expiredUnseenCtx) Err() error {
	if !time.Now().Before(c.at) {
		return context.DeadlineExceeded
	}
	return nil
}

func TestDialContext_DeadlineAsWaitEndsDialsNothing(t *testing.T) {
	for _, closeGroup := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait times out", true: "group closes"}[closeGroup], func(t *testing.T) {
			deadlineAsWaitEnds(t, closeGroup)
		})
	}
}

func deadlineAsWaitEnds(t *testing.T, closeGroup bool) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(5 * time.Second), nil }
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		last := members["a"]
		ctx := expiredUnseenCtx{Context: context.Background(), at: time.Now().Add(time.Second)}
		if closeGroup {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				s.Close()
			}()
		}
		_, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Same(t, last, members["a"], "no member is dialed with a context that ended during the wait")
		time.Sleep(10 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_UDPOutageIsSuspected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		for _, tag := range []string{"a", "b"} {
			obs[tag].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("network down") }
		}
		generation := s.networkGeneration.Load()
		s.noteUserResponse("a", generation, "udp", "dns:53", time.Now(), time.Now())
		time.Sleep(time.Second)
		for range 2 {
			s.noteUnanswered("a", generation, "udp", "dns:53", time.Now(), time.Now())
			time.Sleep(2 * time.Second)
		}
		synctest.Wait()
		s.access.Lock()
		defer s.access.Unlock()
		assert.True(t, s.outageSuspectedLocked(), "with the selection's own probe failing too, nothing is reachable")
	})
}

func TestListenPacket_SessionLeftOnReleasedMemberMovesOnceStuck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		// Once released, a is selected for neither network.
		s.stickyTag.tcp.Store("b")
		handles := map[string][]*scriptedPacketConn{}
		listenScripted(obs, handles, "a")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		dest := metadata.ParseSocksaddrHostPort("dns.test", 53)
		var pcs []net.PacketConn
		for i := range 2 {
			pc, err := s.ListenPacket(context.Background(), dest)
			require.NoError(t, err)
			go func() {
				for {
					if _, _, err := pc.ReadFrom(make([]byte, 64)); err != nil {
						return
					}
				}
			}()
			pc.WriteTo([]byte("q"), &net.UDPAddr{})
			handles["a"][i].in <- []byte("a")
			pcs = append(pcs, pc)
		}
		time.Sleep(time.Second)
		for range 2 {
			pcs[0].WriteTo([]byte("q"), &net.UDPAddr{})
			time.Sleep(2250 * time.Millisecond)
		}
		synctest.Wait()
		require.Equal(t, "b", loadString(&s.stickyTag.udp))
		require.True(t, isClosed(handles["a"][0]))
		require.False(t, isClosed(handles["a"][1]), "a session idle at the release is left")

		pcs[1].WriteTo([]byte("q"), &net.UDPAddr{})
		time.Sleep(2250 * time.Millisecond)
		synctest.Wait()
		assert.True(t, isClosed(handles["a"][1]), "once its exchange goes unanswered it is moved off the released member too")
		for _, pc := range pcs {
			pc.Close()
		}
	})
}

func TestDialContext_CanceledContextDialsNothing(t *testing.T) {
	s, obs := newConfirmationTestMUR(t, "a", "b")
	var dials atomic.Int32
	for _, tag := range []string{"a", "b"} {
		obs[tag].dial = func(ctx context.Context) (net.Conn, error) {
			dials.Add(1)
			return nil, ctx.Err()
		}
		obs[tag].listen = func(ctx context.Context) (net.PacketConn, error) {
			dials.Add(1)
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.ListenPacket(ctx, metadata.Socksaddr{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, dials.Load(), "a caller already gone dials no member")
	failures, _ := userFailures(s, "a")
	assert.Empty(t, failures, "and charges none")
}

func TestDialContext_GroupClosingDuringWaitDialsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, members := newStreakTestMUR(t)
		ob, _ := s.members.Load("b")
		ob.(*mockOutbound).dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(5 * time.Second), nil }
		unansweredExchange(t, s, members, false, 3*time.Second)
		unansweredExchange(t, s, members, false, 3*time.Second)
		last := members["a"]
		go func() {
			time.Sleep(time.Second)
			s.Close()
		}()
		_, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.ErrorIs(t, err, net.ErrClosed)
		assert.Same(t, last, members["a"], "no member is dialed once the group has closed")
		time.Sleep(10 * time.Second)
		synctest.Wait()
	})
}

func TestConfirmation_SlowReplacementProbeStillSwitches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.probeTimeouts.Store(&map[string]time.Duration{"slow": time.Minute})
		obs["b"].typeName = "slow"
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(40 * time.Second), nil }
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(time.Minute)
		synctest.Wait()
		require.EqualValues(t, 2, aProbes.Load(), "both confirmations failed")
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp),
			"a replacement probe still running when the strike window ends can conclude the switch")
	})
}

func TestConfirmation_GatedPassingReplacementIsNoOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.access.Lock()
		s.historyForLocked("b").startRecoveryGate(time.Now())
		s.access.Unlock()
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		require.EqualValues(t, 2, aProbes.Load(), "both confirmations failed")
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp), "a replacement in its recovery gate can't take the selection")
		s.access.Lock()
		defer s.access.Unlock()
		assert.False(t, s.outageSuspectedLocked(), "but its passing probe shows the network is up")
	})
}

func TestDataPlaneStream_ReplyDisarmsTimersBeforeHooks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var failures, slow atomic.Int32
		c := newScriptedConn()
		d := newDataPlaneStream(c, time.Hour, 1<<20, dataPlaneHooks{
			onResponse:     func(time.Time, string) { <-release },
			onFailure:      func(adapter.UserFailureKind) { failures.Add(1) },
			onSlowResponse: func() { slow.Add(1) },
		})
		d.firstResponseTimeout = defaultFirstResponseTimeout
		d.slowResponseAfter = earlyConfirmAfter
		defer d.Close()
		d.Write([]byte("request"))
		c.reads <- scriptedRead{data: []byte("reply")}
		go d.Read(make([]byte, 8))
		time.Sleep(defaultFirstResponseTimeout + time.Second)
		synctest.Wait()
		assert.Zero(t, slow.Load(), "a reply whose hooks are still running is no slow response")
		assert.Zero(t, failures.Load(), "nor a missing one")
		close(release)
	})
}

func TestConfirmation_ClosesUnwrittenConns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
		user := newScriptedConn()
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, func(context.Context) (net.Conn, error) { return user, nil })
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		closed := make(chan struct{})
		go func() {
			conn.Read(make([]byte, 8))
			close(closed)
		}()
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		synctest.Wait()
		require.Equal(t, "b", loadString(&s.stickyTag.tcp))
		select {
		case <-closed:
		default:
			t.Fatal("a conn that hasn't written yet to a failed member is closed")
		}
	})
}

func TestAdvanceNetwork_ClearsFailureStreaks(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	recordSuccess(s, "a", 100)
	s.stickyTag.tcp.Store("a")
	generation := s.networkGeneration.Load()
	s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
	s.noteUnanswered("a", generation, "tcp", "", time.Now().Add(time.Second), time.Now().Add(time.Second))
	s.access.Lock()
	defer s.access.Unlock()
	h := s.historyForLocked("a")
	require.EqualValues(t, 2, h.streakCount(streakTCP, time.Now().Add(time.Second), s.hist.userFailureWindow))
	s.lastStreakSwitch.tcp = time.Now()
	s.advanceNetworkLocked()
	assert.Zero(t, h.streakCount(streakTCP, time.Now().Add(time.Second), s.hist.userFailureWindow),
		"an old network's failure streak says nothing about the new one")
	assert.True(t, s.lastStreakSwitch.tcp.IsZero(), "nor does its release cooldown")
}

func TestConfirmation_PauseDropsPendingSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.probeTimeouts.Store(&map[string]time.Duration{"slow": time.Minute})
		obs["b"].typeName = "slow"
		var aProbes atomic.Int32
		failingProbes(obs["a"], &aProbes, hangUntilDone)
		var woke atomic.Bool
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			if woke.Load() {
				return probeResponderConn(), nil
			}
			return slowProbeResponderConn(40 * time.Second), nil
		}
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(10 * time.Second)
		require.EqualValues(t, 2, aProbes.Load(), "both confirmations failed")
		s.onProbePause(pause.EventNetworkPause)
		time.Sleep(time.Hour)
		woke.Store(true)
		s.onProbePause(pause.EventNetworkWake)
		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp), "a switch decided before a pause doesn't act on probes after it")
	})
}

func TestConfirmation_GatedPassAmongFailedReplacementsIsNoOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		var aProbes, cProbes atomic.Int32
		// a's probes time out, leaving time for b's to pass between them.
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			aProbes.Add(1)
			return hangUntilDone(ctx)
		}
		failingProbes(obs["c"], &cProbes, hangUntilDone)
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.access.Lock()
		s.historyForLocked("b").startRecoveryGate(time.Now())
		s.access.Unlock()
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(defaultProbeTimeout + time.Second)
		s.access.Lock()
		s.requestProbesLocked([]string{"b"}, probeForced, false)
		s.dispatchProbesLocked()
		s.access.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.EqualValues(t, 2, aProbes.Load(), "both confirmations failed")
		require.NotZero(t, cProbes.Load(), "the usable replacement failed its probe")
		s.access.Lock()
		defer s.access.Unlock()
		assert.False(t, s.outageSuspectedLocked(), "a member in its recovery gate passing shows the network is up")
	})
}

func TestConfirmation_RecheckStartsAfterReplacementPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		// a's first two probes time out; later ones pass.
		var aProbes atomic.Int32
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			if aProbes.Add(1) <= 2 {
				return hangUntilDone(ctx)
			}
			return probeResponderConn(), nil
		}
		var bProbes atomic.Int32
		failingProbes(obs["b"], &bProbes, hangUntilDone)
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(time.Second), nil }
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(time.Minute)
		synctest.Wait()
		require.GreaterOrEqual(t, aProbes.Load(), int32(3), "a is rechecked")
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp),
			"a probe of a started before c passed is no recheck after it; the recheck that follows passes")
	})
}

func TestDialContext_CallerCancelIsNotCharged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.lastLadderAt.Store(0)
		obs["a"].dial = hangUntilDone
		var bProbes atomic.Int32
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) {
			bProbes.Add(1)
			return probeResponderConn(), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(500 * time.Millisecond)
			cancel()
		}()
		_, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		require.Error(t, err)
		synctest.Wait()
		failures, _ := userFailures(s, "a")
		assert.Empty(t, failures, "a caller giving up says nothing about the member")
		assert.Zero(t, bProbes.Load(), "so it starts no ladder")
	})
}

type failingSendsPacketConn struct {
	*scriptedPacketConn
	d     *dataPlanePacket
	sends int
}

func (c *failingSendsPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.sends++
	if c.sends == 1 {
		c.d.WriteTo(p, addr)
	}
	return 0, os.ErrDeadlineExceeded
}

func TestDataPlanePacket_AllSendsFailingEndsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unanswered atomic.Int32
		c := &failingSendsPacketConn{scriptedPacketConn: newScriptedPacketConn()}
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onUnanswered: func(time.Time, string) { unanswered.Add(1) },
		})
		c.d = d
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		d.WriteTo([]byte("query"), &net.UDPAddr{})
		time.Sleep(2 * defaultUDPUnansweredAfter)
		synctest.Wait()
		assert.Zero(t, unanswered.Load(), "no datagram was sent, so none went unanswered")
	})
}

func TestConfirmation_PauseDropsFirstStrike(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aProbes atomic.Int32
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			aProbes.Add(1)
			return hangUntilDone(ctx)
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
		time.Sleep(defaultProbeTimeout + time.Second)
		s.onProbePause(pause.EventNetworkPause)
		time.Sleep(time.Second)
		s.onProbePause(pause.EventNetworkWake)
		time.Sleep(time.Minute)
		synctest.Wait()
		require.GreaterOrEqual(t, aProbes.Load(), int32(3), "a is probed again after the wake")
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp),
			"a failure before a pause doesn't pair with one after it")
	})
}

func TestSplitHealthy_StreakBumpKeepsTierOrder(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "c")
	recordSuccess(s, "a", 30)
	recordSuccess(s, "b", 500)
	recordSuccess(s, "c", 20)
	s.stickyTag.tcp.Store("a")
	addUserFailureN(s, "b", int(s.hist.softFailLimit))
	addUserFailureN(s, "c", int(s.hist.softFailLimit))
	generation := s.networkGeneration.Load()
	s.noteUnanswered("a", generation, "tcp", "", time.Now(), time.Now())
	s.noteUnanswered("a", generation, "tcp", "", time.Now().Add(time.Second), time.Now().Add(time.Second))
	s.access.Lock()
	defer s.access.Unlock()
	pool, _ := s.splitHealthyForLocked(s.rankLocked(time.Now().Add(time.Second)), "tcp")
	require.Len(t, pool, 3)
	assert.Equal(t, []string{"c", "a", "b"}, []string{pool[0].tag, pool[1].tag, pool[2].tag},
		"a member bumped by its streak ranks within its new tier as any other")
}

func TestAdvanceNetwork_ClearsOutageSuspicion(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	s.access.Lock()
	defer s.access.Unlock()
	s.suspectOutageLocked(time.Now())
	s.advanceNetworkLocked()
	assert.False(t, s.outageSuspectedLocked(), "an outage of the old network says nothing about the new one")
}

func TestStreak_TCPOnlyPassIsNoUDPOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		s.stickyTag.udp.Store("a")
		// a's and b's probes time out, so c's pass comes before they fail.
		for _, tag := range []string{"a", "b"} {
			obs[tag].dial = hangUntilDone
		}
		obs["c"].networks = []string{"tcp"}
		obs["c"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		generation := s.networkGeneration.Load()
		s.noteUserResponse("a", generation, "udp", "dns:53", time.Now(), time.Now())
		time.Sleep(time.Second)
		s.noteUnanswered("a", generation, "udp", "dns:53", time.Now(), time.Now())
		time.Sleep(2 * time.Second)
		s.noteUnanswered("a", generation, "udp", "dns:53", time.Now(), time.Now())
		s.access.Lock()
		s.requestProbesLocked([]string{"c"}, probeForced, false)
		s.dispatchProbesLocked()
		s.access.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		s.access.Lock()
		defer s.access.Unlock()
		require.True(t, s.probeMembers["c"].outcome.success, "c passed")
		assert.False(t, s.outageSuspectedLocked(), "a TCP-only member passing shows the network is up")
	})
}

func TestDialContext_ClosedGroupDialsNothing(t *testing.T) {
	s, obs := newConfirmationTestMUR(t, "a", "b")
	var dials atomic.Int32
	for _, tag := range []string{"a", "b"} {
		obs[tag].dial = func(context.Context) (net.Conn, error) {
			dials.Add(1)
			return newScriptedConn(), nil
		}
	}
	s.Close()
	_, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.ErrorIs(t, err, net.ErrClosed)
	assert.Zero(t, dials.Load(), "a closed group dials no member")
}

// reroutedSendPacketConn fails its first send after another send, a second
// later and to another destination, has joined the wait and succeeded.
type reroutedSendPacketConn struct {
	*scriptedPacketConn
	d        *dataPlanePacket
	to       net.Addr
	sends    int
	joinedAt time.Time
}

func (c *reroutedSendPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.sends++
	if c.sends == 1 {
		time.Sleep(time.Second)
		c.joinedAt = time.Now()
		c.d.WriteTo(p, c.to)
		return 0, errors.New("send failed")
	}
	return len(p), nil
}

func TestDataPlanePacket_WaitFollowsTheSendThatSucceeded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reportedSince time.Time
		var reportedDest string
		c := &reroutedSendPacketConn{
			scriptedPacketConn: newScriptedPacketConn(),
			to:                 &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 53},
		}
		d := newDataPlanePacket(c, time.Hour, 1<<20, dataPlaneHooks{
			onUnanswered: func(since time.Time, dest string) { reportedSince, reportedDest = since, dest },
		})
		c.d = d
		d.unansweredAfter = defaultUDPUnansweredAfter
		d.unansweredWhileOpen = true
		defer d.Close()
		d.WriteTo([]byte("query"), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 53})
		time.Sleep(2 * defaultUDPUnansweredAfter)
		synctest.Wait()
		assert.Equal(t, "10.0.0.2:53", reportedDest, "the wait is for the datagram that was sent")
		assert.Equal(t, c.joinedAt, reportedSince, "from when it was sent")
	})
}

func TestDialContext_ConnMissingTheSwitchSweepIsReplaced(t *testing.T) {
	s, obs := newConfirmationTestMUR(t, "a", "b")
	s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
	obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
		// The selection moves off a after its dial result is taken but
		// before the conn is registered, so the switch's sweep misses it.
		s.access.Lock()
		s.recordProbeOutcomeLocked("a", &probeOutcome{generation: s.networkGeneration.Load(), url: s.probeURLForLocked("a")})
		s.stickyTag.tcp.Store("b")
		s.access.Unlock()
		return newScriptedConn(), nil
	}
	var bDials atomic.Int32
	obs["b"].dial = func(context.Context) (net.Conn, error) {
		bDials.Add(1)
		return newScriptedConn(), nil
	}
	conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	defer conn.Close()
	assert.EqualValues(t, 1, bDials.Load(), "the caller gets a conn through the new selection")
}
