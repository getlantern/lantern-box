//go:build !windows

package group

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
	"github.com/sagernet/sing/common/metadata"
)

func TestPeerAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"reset", opErr("read", syscall.ECONNRESET), true},
		{"refused", opErr("connect", syscall.ECONNREFUSED), true},
		{"wrapped reset", fmt.Errorf("relay: %w", opErr("read", syscall.ECONNRESET)), true},
		{"aborted by the device", opErr("write", syscall.ECONNABORTED), false},
		{"network unreachable", opErr("connect", syscall.ENETUNREACH), false},
		{"host unreachable", opErr("connect", syscall.EHOSTUNREACH), false},
		{"timeout", timeoutErr{}, false},
		{"none", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, peerAnswered(tc.err))
		})
	}
}

func TestFailedLocally(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"network unreachable", opErr("write", syscall.ENETUNREACH), true},
		{"network down", opErr("connect", syscall.ENETDOWN), true},
		{"aborted by the device", opErr("read", syscall.ECONNABORTED), true},
		{"host unreachable", opErr("connect", syscall.EHOSTUNREACH), false},
		{"reset", opErr("read", syscall.ECONNRESET), false},
		{"timeout", timeoutErr{}, false},
		{"none", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, failedLocally(tc.err))
		})
	}
}

func TestOutage_LocalFailuresAreNotCounted(t *testing.T) {
	s, _ := newConfirmationTestMUR(t, "a", "b")
	s.hist.userFailureDedupeWindow = 0
	s.access.Lock()
	s.suspectOutageLocked(time.Now())
	s.access.Unlock()
	generation := s.networkGeneration.Load()
	for _, cause := range []error{opErr("connect", syscall.ENETUNREACH), opErr("write", syscall.ECONNABORTED), nil} {
		assert.False(t, s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureDial, time.Now(), "tcp", cause))
	}
	failures, _ := userFailures(s, "a")
	assert.Empty(t, failures, "failures of the device's own network don't demote the member")
	s.access.Lock()
	defer s.access.Unlock()
	assert.True(t, s.outageSuspectedLocked())
}

func TestOutage_PeerResetEndsSuspicionAndCounts(t *testing.T) {
	s, _ := newConfirmationTestMUR(t, "a", "b")
	s.access.Lock()
	s.suspectOutageLocked(time.Now())
	s.access.Unlock()
	opened := time.Now()
	require.True(t, s.recordUserFailure(s.originAt("a", s.networkGeneration.Load()), adapter.UserFailureReset, opened, "tcp", opErr("read", syscall.ECONNRESET)))
	failures, _ := userFailures(s, "a")
	assert.Len(t, failures, 1, "a reset from the far end is the member's failure")
	s.access.Lock()
	defer s.access.Unlock()
	assert.False(t, s.outageSuspectedLocked(), "and shows the device's network is up")
}

func TestOutage_PeerResetOnAConnTheOutageOutlastedIsNotCounted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ended bool
	}{
		{"during the suspicion", false},
		{"after it ended", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := newConfirmationTestMUR(t, "a", "b")
				opened := time.Now()
				time.Sleep(time.Second)
				s.access.Lock()
				s.suspectOutageLocked(opened)
				if tc.ended {
					s.endOutageLocked()
				}
				s.access.Unlock()
				time.Sleep(time.Second)
				assert.False(t, s.recordUserFailure(s.originAt("a", s.networkGeneration.Load()), adapter.UserFailureReset, opened, "tcp", opErr("write", syscall.ECONNRESET)),
					"the far end may have dropped the conn during the outage")
				failures, _ := userFailures(s, "a")
				assert.Empty(t, failures)
				s.access.Lock()
				defer s.access.Unlock()
				assert.False(t, s.outageSuspectedLocked(), "the reset still shows the network is up")
			})
		})
	}
}

func TestOutage_LocalFailureOnAConnTheOutageOutlastedIsNotCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newConfirmationTestMUR(t, "a", "b")
		opened := time.Now()
		time.Sleep(time.Second)
		s.access.Lock()
		s.suspectOutageLocked(opened)
		s.endOutageLocked()
		s.access.Unlock()
		time.Sleep(time.Second)
		assert.False(t, s.recordUserFailure(s.originAt("a", s.networkGeneration.Load()), adapter.UserFailureStall, opened, "tcp", nil))
		failures, _ := userFailures(s, "a")
		assert.Empty(t, failures, "a stall on a conn opened before the outage ended is the outage's")
	})
}

func TestDataPlaneStream_ResetPassesItsCause(t *testing.T) {
	var got atomic.Value
	c := &failingConn{err: connResetErr(), firstN: 16}
	d := newDataPlaneStream(c, time.Hour, 1<<20, dataPlaneHooks{onFailure: func(_ adapter.UserFailureKind, cause error) { got.Store(cause) }})
	defer d.Close()
	buf := make([]byte, 32)
	_, err := d.Read(buf)
	require.NoError(t, err)
	_, err = d.Read(buf)
	require.Error(t, err)
	require.Eventually(t, func() bool { return got.Load() != nil }, time.Second, 5*time.Millisecond)
	assert.True(t, peerAnswered(got.Load().(error)))
}

func TestStreak_ReleasedWhenOnlyTheProbesFail(t *testing.T) {
	reset := opErr("write", syscall.ECONNRESET)
	for _, tc := range []struct {
		name   string
		causes []error
		// bSoft soft-demotes b first, so the selection and b share a tier.
		bSoft bool
		want  string
	}{
		{"the far end reset the selection's traffic", []error{reset, reset}, false, "b"},
		{"the far end never answered the selection's traffic", []error{nil, nil}, false, "a"},
		{"the far end answered only before the streak", []error{reset, nil}, false, "a"},
		// As when every server refuses: b failing too, nothing sets it apart.
		{"the only other member is in the selection's tier", []error{reset, reset}, true, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newConfirmationTestMUR(t, "a", "b")
				for _, ob := range obs {
					ob.dial = func(context.Context) (net.Conn, error) { return nil, errors.New("probe endpoint down") }
				}
				generation := s.networkGeneration.Load()
				if tc.bSoft {
					for range s.hist.softFailLimit {
						require.True(t, s.recordUserFailure(s.originAt("b", generation), adapter.UserFailureDial, time.Now(), "tcp", nil))
						time.Sleep(s.hist.userFailureDedupeWindow + time.Second)
					}
				}
				for _, cause := range tc.causes {
					s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "tcp", cause)
					time.Sleep(time.Second)
				}
				for range 4 {
					_, _, err := s.selectOutbound("tcp", "")
					require.NoError(t, err)
					if tc.want == "a" {
						require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "the selection is never released")
					}
					time.Sleep(time.Second)
					synctest.Wait()
				}
				assert.Equal(t, tc.want, loadString(&s.selections[streakTCP].tag))
			})
		})
	}
}

// localWriteFailConn's writes fail as a deferred setup's first write does
// once the device has lost its network.
type localWriteFailConn struct{ *scriptedConn }

func (c localWriteFailConn) Write([]byte) (int, error) { return 0, opErr("write", syscall.ENETUNREACH) }

func TestOutage_NoTrialOnceTheSelectionFailsLocallyOrAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		// conn is a's conn; answer has it answer after it went unanswered.
		conn   func() net.Conn
		answer bool
	}{
		{"a deferred setup's write fails locally", func() net.Conn { return localWriteFailConn{newScriptedConn()} }, false},
		{"the selection answers again", func() net.Conn { return newScriptedConn() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newConfirmationTestMUR(t, "a", "b")
				probeDown := errors.New("probe endpoint down")
				obs["a"].dialTo = func(_ context.Context, dest metadata.Socksaddr) (net.Conn, error) {
					if dest.Fqdn == "probe.test" {
						return nil, probeDown
					}
					return tc.conn(), nil
				}
				obs["b"].dialTo = func(_ context.Context, dest metadata.Socksaddr) (net.Conn, error) {
					if dest.Fqdn == "probe.test" {
						return nil, probeDown
					}
					return probeResponderConn(), nil
				}
				generation := s.networkGeneration.Load()
				s.noteUnanswered(s.originAt("a", generation), "tcp", "", time.Now(), time.Now())
				conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
				require.NoError(t, err)
				conn.Write([]byte("request"))
				conn.Close()
				if tc.answer {
					s.noteUserResponse(s.originAt("a", generation), "tcp", "", time.Now(), time.Now())
				}
				s.access.Lock()
				s.suspectOutageLocked(time.Now())
				s.access.Unlock()
				time.Sleep(3 * time.Second)
				conn, err = s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
				if err == nil {
					defer time.AfterFunc(5*time.Second, func() { conn.Close() }).Stop()
					conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
					conn.Read(make([]byte, 64))
					conn.Close()
				}
				synctest.Wait()
				assert.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "nothing showed a failing while the network was up")
			})
		})
	}
}

func TestDial_AlternateTheDeviceFailedIsNotCharged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newDialTestMUR(t)
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return nil, opErr("connect", syscall.ENETUNREACH) }
		_, err := s.DialContext(context.Background(), "tcp", metadata.ParseSocksaddr("x.test:443"))
		require.Error(t, err)
		synctest.Wait()
		uf, _ := userFailures(s, "b")
		assert.Empty(t, uf, "the device, not b, failed b's dial")
	})
}

func TestDataPlane_ConnTheDeviceAbortedIsNotCharged(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	s.selections[streakTCP].tag.Store("a")
	report := s.makeHooks(s.originAt("a", s.networkGeneration.Load()), "tcp")
	report.failure(adapter.UserFailureReset, opErr("read", syscall.ECONNABORTED))
	uf, _ := userFailures(s, "a")
	assert.Empty(t, uf)
}

func TestDial_MemberTheDeviceCannotRouteToFailsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newDialTestMUR(t)
		// As with an IPv6 server on an IPv4-only network.
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, opErr("connect", syscall.ENETUNREACH) }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return echoConn{}, nil }
		conn, err := s.DialContext(context.Background(), "tcp", metadata.ParseSocksaddr("x.test:443"))
		require.NoError(t, err, "b, which the device can reach, serves")
		conn.Close()
		synctest.Wait()
		uf, _ := userFailures(s, "a")
		assert.Empty(t, uf, "the device failed a's dial")
	})
}
