package group

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
)

func refusedErr() error {
	return &net.OpError{Op: "read", Net: "udp", Err: syscall.ECONNREFUSED}
}

func TestDataPlane_QuietReadPolicy(t *testing.T) {
	tests := []struct {
		name    string
		last    string
		quiet   time.Duration
		isRead  bool
		n       int
		proven  bool
		excused bool
	}{
		{"past threshold", "read", 10 * time.Second, true, 0, false, true},
		{"at threshold", "read", defaultDataPlaneResetQuiet, true, 0, false, true},
		{"under threshold", "read", defaultDataPlaneResetQuiet - time.Nanosecond, true, 0, false, false},
		{"proven quiet read", "read", 10 * time.Second, true, 0, true, true},
		{"failed write", "read", 10 * time.Second, false, 0, false, false},
		{"bytes with error", "read", 10 * time.Second, true, 8, false, false},
		{"no prior IO", "none", 10 * time.Second, true, 0, false, false},
		{"unanswered write", "write", 10 * time.Second, true, 0, false, false},
		{"recent read", "read", time.Second, true, 0, false, false},
	}
	errs := map[string]error{
		"reset":   fmt.Errorf("vless: %w", connResetErr()),
		"refused": fmt.Errorf("shadowsocks: %w", refusedErr()),
		"closed":  fmt.Errorf("mux: %w", net.ErrClosed),
	}
	for errName, readErr := range errs {
		for _, tt := range tests {
			t.Run(errName+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var w dataPlaneWatchdog
					var failures []adapter.UserFailureKind
					var observed dataPlaneIO
					var activity, observations int
					w.init(time.Hour, defaultDataPlaneProvedReadBytes, dataPlaneHooks{
						onFailure:  func(kind adapter.UserFailureKind) { failures = append(failures, kind) },
						onActivity: func() { activity++ },
						onError: func(err error, state dataPlaneIO, excused bool) {
							assert.Same(t, readErr, err)
							assert.Equal(t, tt.excused, excused)
							assert.Equal(t, state, w.snapshotIO(), "diagnostics must run outside the IO lock")
							observed = state
							observations++
						},
					})
					defer w.closeWatchdog()
					if tt.last != "none" {
						n := 48
						if tt.proven {
							n = defaultDataPlaneProvedReadBytes
						}
						w.noteIO(n, nil, tt.last == "read")
					}
					time.Sleep(tt.quiet)
					before, activityBefore := w.snapshotIO(), activity
					w.noteIO(tt.n, readErr, tt.isRead)
					synctest.Wait()

					assert.Equal(t, 1, observations)
					assert.Equal(t, before, observed, "diagnostics must describe the IO before the error")
					assert.Equal(t, tt.quiet, observed.quiet)
					assert.Equal(t, tt.proven, observed.proven)
					assert.Equal(t, before, w.snapshotIO(), "errors must not update IO state")
					assert.Equal(t, activityBefore, activity, "errors must not count as activity")
					assert.Equal(t, !tt.excused, w.fired.Load())
					assert.Equal(t, !tt.excused, w.stalled.Load())
					if tt.excused {
						assert.Empty(t, failures)
					} else {
						assert.Equal(t, []adapter.UserFailureKind{adapter.UserFailureReset}, failures)
					}
				})
			})
		}
	}
}

func TestDataPlane_ExcusedFailureThenLiveResetChargedOnce(t *testing.T) {
	for _, isRead := range []bool{true, false} {
		t.Run(fmt.Sprintf("next-read=%t", isRead), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var failures []adapter.UserFailureKind
				var decisions []bool
				var w dataPlaneWatchdog
				w.init(time.Hour, defaultDataPlaneProvedReadBytes, dataPlaneHooks{
					onFailure: func(kind adapter.UserFailureKind) { failures = append(failures, kind) },
					onError:   func(_ error, _ dataPlaneIO, excused bool) { decisions = append(decisions, excused) },
				})
				defer w.closeWatchdog()
				w.noteIO(48, nil, true)
				time.Sleep(defaultDataPlaneResetQuiet)
				w.noteIO(0, connResetErr(), true)
				require.False(t, w.fired.Load())

				w.noteIO(8, nil, isRead)
				w.noteIO(0, connResetErr(), true)
				w.noteIO(0, connResetErr(), true)
				synctest.Wait()
				assert.Equal(t, []adapter.UserFailureKind{adapter.UserFailureReset}, failures)
				assert.Equal(t, []bool{true, false}, decisions)
			})
		})
	}
}

func TestDataPlane_NoDiagnosticsAfterClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var w dataPlaneWatchdog
		w.init(time.Hour, 0, dataPlaneHooks{
			onFailure: func(adapter.UserFailureKind) { t.Error("failure after close") },
			onError:   func(error, dataPlaneIO, bool) { t.Error("diagnostic after close") },
		})
		w.closeWatchdog()
		w.noteIO(0, net.ErrClosed, true)
		synctest.Wait()
		assert.False(t, w.fired.Load())
	})
}

func TestDataPlane_ConcurrentIO(t *testing.T) {
	var w dataPlaneWatchdog
	w.init(time.Hour, defaultDataPlaneProvedReadBytes, dataPlaneHooks{})
	defer w.closeWatchdog()
	var wg sync.WaitGroup
	for _, isRead := range []bool{true, false} {
		wg.Go(func() {
			for range 1000 {
				w.noteIO(1, nil, isRead)
			}
		})
	}
	for range 1000 {
		assert.GreaterOrEqual(t, w.snapshotIO().quiet, time.Duration(0))
	}
	wg.Wait()
	assert.True(t, w.snapshotIO().hasIO)
}

func TestDataPlane_ConcurrentFailureClose(t *testing.T) {
	for _, closeEarly := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeEarly), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Uint32
				var w dataPlaneWatchdog
				w.init(time.Hour, 1, dataPlaneHooks{
					onFailure: func(adapter.UserFailureKind) { calls.Add(1) },
				})
				defer w.closeWatchdog()
				w.noteIO(1, nil, true)
				w.noteIO(1, nil, false)
				var wg sync.WaitGroup
				for range 20 {
					wg.Go(func() { w.noteIO(0, connResetErr(), true) })
					wg.Go(w.fireStall)
					if closeEarly {
						wg.Go(func() { w.closeWatchdog() })
					}
				}
				wg.Wait()
				synctest.Wait()
				if closeEarly {
					assert.LessOrEqual(t, calls.Load(), uint32(1))
				} else {
					assert.Equal(t, uint32(1), calls.Load())
				}
				before := calls.Load()
				w.closeWatchdog()
				w.noteIO(0, net.ErrClosed, true)
				synctest.Wait()
				assert.Equal(t, before, calls.Load())
			})
		})
	}
}

func TestDataPlaneStream_QuietReadErrorReturned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		readErr := connResetErr()
		d := newDataPlaneStream(&failingConn{firstN: 48, err: readErr}, time.Hour,
			defaultDataPlaneProvedReadBytes, dataPlaneHooks{})
		defer d.Close()
		_, err := d.Read(make([]byte, 64))
		require.NoError(t, err)
		time.Sleep(defaultDataPlaneResetQuiet)
		_, err = d.Read(make([]byte, 64))
		require.Same(t, readErr, err)
		assert.False(t, d.fired.Load())
	})
}

func TestDialContext_UDPConnectQuietReadExcused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newTestMUR(t, "a")
		recordSuccess(s, "a", 10)
		s.cfg.ladderCooldown = time.Hour
		s.lastLadderAt.Store(time.Now().UnixNano())
		readErr := refusedErr()
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			return &failingConn{firstN: 48, err: readErr}, nil
		}
		quiet, err := s.DialContext(context.Background(), "udp", metadata.Socksaddr{})
		require.NoError(t, err)
		defer quiet.Close()
		_, err = quiet.Read(make([]byte, 64))
		require.NoError(t, err)
		time.Sleep(defaultDataPlaneResetQuiet)
		_, err = quiet.Read(make([]byte, 64))
		require.Same(t, readErr, err)
		synctest.Wait()
		uf, _ := userFailures(s, "a")
		require.Empty(t, uf)

		obs["a"].dial = func(context.Context) (net.Conn, error) {
			return &failingConn{err: readErr}, nil
		}
		live, err := s.DialContext(context.Background(), "udp", metadata.Socksaddr{})
		require.NoError(t, err)
		defer live.Close()
		_, err = live.Read(make([]byte, 8))
		require.Same(t, readErr, err)
		synctest.Wait()
		uf, _ = userFailures(s, "a")
		require.Len(t, uf, 1)
		assert.Equal(t, adapter.UserFailureReset, uf[0].Kind)
	})
}

func TestListenPacket_QuietReadExcused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newTestMUR(t, "a")
		recordSuccess(s, "a", 10)
		readErr := connResetErr()
		obs["a"].On("ListenPacket").Return(&failingPacketConn{err: readErr}, nil)
		pc, err := s.ListenPacket(context.Background(), metadata.Socksaddr{})
		require.NoError(t, err)
		defer pc.Close()
		d := pc.(*adapter.TaggedPacketConn).PacketConn.(*dataPlanePacket)
		d.noteIO(48, nil, true)
		time.Sleep(defaultDataPlaneResetQuiet)
		_, _, err = pc.ReadFrom(make([]byte, 8))
		require.Same(t, readErr, err)
		assert.False(t, d.fired.Load())
		synctest.Wait()
		uf, _ := userFailures(s, "a")
		assert.Empty(t, uf)
	})
}
