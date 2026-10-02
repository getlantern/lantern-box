package group

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
)

func probeResponderConn() net.Conn {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		io.WriteString(server, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
	}()
	return client
}

// newConfirmationTestMUR selects a and suppresses ladder probes.
func newConfirmationTestMUR(t *testing.T, tags ...string) (*MutableAutoSelect, map[string]*mockOutbound) {
	t.Helper()
	s, obs := newTestMUR(t, tags...)
	s.defaultURL = "http://probe.test/"
	s.cfg.ladderCooldown = time.Hour
	s.lastLadderAt.Store(time.Now().UnixNano())
	for i, tag := range tags {
		recordSuccess(s, tag, uint32(10*(i+1)))
	}
	s.stickyTag.tcp.Store("a")
	return s, obs
}

func TestDialContext_FailedConfirmationSwitchesToFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		// Slower than a by more than switchTolerance, so only the gate keeps
		// a from winning back on delay.
		recordSuccess(s, "b", 200)
		var aDials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			aDials.Add(1)
			return nil, errors.New("dial timeout")
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			return probeResponderConn(), nil
		}

		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		synctest.Wait()

		assert.Equal(t, "b", loadString(&s.stickyTag.tcp), "a failed confirmation, so b serves subsequent dials")
		assert.EqualValues(t, 2, aDials.Load(), "the failed dial plus one confirmation probe")

		conn, err = s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.stickyTag.tcp), "gated a must not win back on delay")
		assert.EqualValues(t, 2, aDials.Load(), "the next dial goes straight to b")
	})
}

func TestDialContext_PassingConfirmationKeepsSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		var aDials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if aDials.Add(1) == 1 {
				return nil, errors.New("transient dial failure")
			}
			return probeResponderConn(), nil
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			return probeResponderConn(), nil
		}

		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		synctest.Wait()

		assert.Equal(t, "a", loadString(&s.stickyTag.tcp))
		h, _ := s.peekHistoryLocked("a")
		assert.False(t, h.hasRecoveryGate())
	})
}

func TestDialContext_UDPFailureDoesNotSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.stickyTag.udp.Store("a")
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			return nil, errors.New("dial refused")
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			return probeResponderConn(), nil
		}

		conn, err := s.DialContext(context.Background(), "udp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		synctest.Wait()

		assert.Equal(t, "a", loadString(&s.stickyTag.udp))
		assert.Equal(t, "a", loadString(&s.stickyTag.tcp))
	})
}

func TestTrySwitchAfterConfirmedFailure(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(s *MutableAutoSelect)
		switched bool
	}{
		{"validated fallback", func(*MutableAutoSelect) {}, true},
		{"current selection changed", func(s *MutableAutoSelect) { s.stickyTag.tcp.Store("c") }, false},
		{"fallback failed its last probe", func(s *MutableAutoSelect) {
			s.recordProbeOutcome("b", false, 0)
		}, false},
		{"fallback never probed", func(s *MutableAutoSelect) {
			s.access.Lock()
			delete(s.histories, "b")
			s.access.Unlock()
		}, false},
		{"rate limited", func(s *MutableAutoSelect) {
			s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = time.Now(), "c"
		}, false},
		{"rate limit expired", func(s *MutableAutoSelect) {
			s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = time.Now().Add(-confirmedFailureSwitchCooldown), "c"
		}, true},
		{"replacement fails again", func(s *MutableAutoSelect) {
			s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = time.Now(), "a"
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := newConfirmationTestMUR(t, "a", "b", "c")
				tt.setup(s)
				before := loadString(&s.stickyTag.tcp)
				s.trySwitchAfterConfirmedFailure("a", "b")

				h, _ := s.peekHistoryLocked("a")
				if tt.switched {
					assert.Equal(t, "b", loadString(&s.stickyTag.tcp))
					assert.True(t, h.hasRecoveryGate())
					assert.Equal(t, "b", s.lastConfirmedFailureSwitch.selectedTag)
				} else {
					assert.Equal(t, before, loadString(&s.stickyTag.tcp))
					assert.False(t, h.hasRecoveryGate())
				}
			})
		})
	}
}

func TestRecoveryGate_SelectionAndClearing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b")
		s.access.Lock()
		s.historyForLocked("a").startRecoveryGate(time.Now())
		s.access.Unlock()

		o, err := s.selectFor("tcp")
		require.NoError(t, err)
		assert.Equal(t, "b", o.Tag(), "a recovery gate overrides stickiness and lower delay")

		s.access.Lock()
		delete(s.histories, "b")
		s.access.Unlock()
		s.members.Delete("b")
		s.stickyTag.tcp.Store("")
		o, err = s.selectFor("tcp")
		require.NoError(t, err)
		assert.Equal(t, "a", o.Tag(), "a gated member still serves when nothing else is available")

		s.members.Store("b", obs["b"])
		recordSuccess(s, "b", 200)
		o, err = s.selectFor("tcp")
		require.NoError(t, err)
		assert.Equal(t, "b", o.Tag(), "the gated sticky yields when an ungated member returns")

		h, _ := s.peekHistoryLocked("a")
		time.Sleep(recoveryGateMinDuration - time.Second)
		s.recordProbeOutcome("a", true, 10)
		assert.True(t, h.hasRecoveryGate(), "a probe success before the minimum duration does not clear the gate")
		time.Sleep(time.Second)
		s.recordProbeOutcome("a", false, 0)
		assert.True(t, h.hasRecoveryGate(), "elapsed time and failed probes do not clear the gate")
		s.recordProbeOutcome("a", true, 10)
		assert.False(t, h.hasRecoveryGate(), "a probe success after the minimum duration clears the gate")
		o, err = s.selectFor("tcp")
		require.NoError(t, err)
		assert.Equal(t, "a", o.Tag(), "a recovered member can win on delay again")
	})
}

func TestRecoveryGate_FallbackDemotionDoesNotRestoreFailedMember(t *testing.T) {
	s, _ := newConfirmationTestMUR(t, "a", "b")
	recordSuccess(s, "b", 200)
	s.recordUserFailure("a", adapter.UserFailureDial)
	s.recordProbeOutcome("a", false, 0)
	s.trySwitchAfterConfirmedFailure("a", "b")
	require.Equal(t, "b", loadString(&s.stickyTag.tcp))

	s.hist.userFailureDedupeWindow = 0
	for range s.hist.softFailLimit {
		s.recordUserFailure("b", adapter.UserFailureStall)
	}
	o, err := s.selectFor("tcp")
	require.NoError(t, err)
	assert.Equal(t, "b", o.Tag())
}

func TestSplitHealthyFor_RecoveryGate(t *testing.T) {
	tests := []struct {
		name        string
		aDemote     demoteLevel
		bGated      bool
		bDemote     demoteLevel
		bNetwork    string
		want        string
		wantMembers int
	}{
		{"ungated clean", demoteClean, false, demoteClean, "tcp", "b", 1},
		{"ungated soft", demoteClean, false, demoteSoft, "tcp", "b", 1},
		{"ungated hard, gated usable", demoteClean, false, demoteHard, "tcp", "a", 2},
		{"ungated hard, gated hard", demoteHard, false, demoteHard, "tcp", "b", 1},
		{"all gated", demoteClean, true, demoteSoft, "tcp", "a", 2},
		{"ungated other network", demoteClean, false, demoteClean, "udp", "a", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &MutableAutoSelect{}
			ranked := []rankedCandidate{
				{outbound: &mockOutbound{networks: []string{"tcp"}}, tag: "a", hasRecoveryGate: true, demote: tt.aDemote},
				{outbound: &mockOutbound{networks: []string{tt.bNetwork}}, tag: "b", hasRecoveryGate: tt.bGated, demote: tt.bDemote},
			}
			pool, forNetwork := s.splitHealthyForLocked(ranked, "tcp")
			require.NotEmpty(t, pool)
			assert.Equal(t, tt.want, pool[0].tag)
			assert.Len(t, forNetwork, tt.wantMembers)
		})
	}
}

func TestRecoveryGate_HardDemotedFallbackRestoresFailedMember(t *testing.T) {
	s, _ := newConfirmationTestMUR(t, "a", "b")
	recordSuccess(s, "b", 200)
	s.recordUserFailure("a", adapter.UserFailureDial)
	s.recordProbeOutcome("a", false, 0)
	s.trySwitchAfterConfirmedFailure("a", "b")
	require.Equal(t, "b", loadString(&s.stickyTag.tcp))

	s.hist.userFailureDedupeWindow = 0
	for range s.hist.consecutiveFailLimit {
		s.recordUserFailure("b", adapter.UserFailureStall)
	}
	o, err := s.selectFor("tcp")
	require.NoError(t, err)
	assert.Equal(t, "a", o.Tag(), "a gated member that is not demoted serves before a hard-demoted fallback")
}

func TestRecoveryGate_FastFailover(t *testing.T) {
	s, _ := newConfirmationTestMUR(t, "a", "b", "c")
	s.historyForLocked("b").startRecoveryGate(time.Now())
	o, err := s.selectForExcluding("tcp", "a")
	require.NoError(t, err)
	assert.Equal(t, "c", o.Tag())
	assert.Equal(t, "a", loadString(&s.stickyTag.tcp))

	s.historyForLocked("c").startRecoveryGate(time.Now())
	o, err = s.selectForExcluding("tcp", "a")
	require.NoError(t, err)
	assert.Equal(t, "b", o.Tag(), "fast failover can use a gated member as a last resort")
}

func TestDataPlaneFailure_ConfirmsSelection(t *testing.T) {
	tests := []struct {
		name     string
		firstA   func() net.Conn
		probeOK  bool
		drive    func(conn net.Conn)
		wantKind adapter.UserFailureKind
		switched bool
	}{
		{
			name:   "no response, probe fails",
			firstA: blackholeConn,
			drive: func(conn net.Conn) {
				conn.Write([]byte("hello"))
				go io.ReadFull(conn, make([]byte, 1))
				time.Sleep(defaultFirstResponseTimeout)
			},
			wantKind: adapter.UserFailureNoResponse,
			switched: true,
		},
		{
			name:     "reset, probe fails",
			firstA:   func() net.Conn { return &failingConn{err: connResetErr()} },
			drive:    func(conn net.Conn) { conn.Read(make([]byte, 1)) },
			wantKind: adapter.UserFailureReset,
			switched: true,
		},
		{
			name:     "reset, probe passes",
			firstA:   func() net.Conn { return &failingConn{err: connResetErr()} },
			probeOK:  true,
			drive:    func(conn net.Conn) { conn.Read(make([]byte, 1)) },
			wantKind: adapter.UserFailureReset,
			switched: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newConfirmationTestMUR(t, "a", "b")
				s.cfg.dataPlaneFirstResponseTimeout = defaultFirstResponseTimeout
				var aDials atomic.Int32
				obs["a"].dial = func(context.Context) (net.Conn, error) {
					if aDials.Add(1) == 1 {
						return tt.firstA(), nil
					}
					if tt.probeOK {
						return probeResponderConn(), nil
					}
					return nil, errors.New("dial timeout")
				}

				conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
				require.NoError(t, err)
				defer conn.Close()
				tt.drive(conn)
				synctest.Wait()

				uf, _ := userFailures(s, "a")
				require.Len(t, uf, 1)
				assert.Equal(t, tt.wantKind, uf[0].Kind)
				assert.EqualValues(t, 2, aDials.Load(), "one user dial plus one confirmation probe")
				h, _ := s.peekHistoryLocked("a")
				if tt.switched {
					assert.Equal(t, "b", loadString(&s.stickyTag.tcp))
					assert.True(t, h.hasRecoveryGate())
				} else {
					assert.Equal(t, "a", loadString(&s.stickyTag.tcp))
					assert.False(t, h.hasRecoveryGate())
				}
			})
		})
	}
}

func TestMakeHooks_ConfirmsOnlyPrimaryTCPResetOrNoResponse(t *testing.T) {
	tests := []struct {
		name     string
		network  string
		route    routeKind
		kind     adapter.UserFailureKind
		confirms bool
	}{
		{"tcp reset", "tcp", primaryRoute, adapter.UserFailureReset, true},
		{"tcp no response", "tcp", primaryRoute, adapter.UserFailureNoResponse, true},
		{"tcp stall", "tcp", primaryRoute, adapter.UserFailureStall, false},
		{"udp reset", "udp", primaryRoute, adapter.UserFailureReset, false},
		{"fallback reset", "tcp", fallbackRoute, adapter.UserFailureReset, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newConfirmationTestMUR(t, "a", "b")
				s.stickyTag.udp.Store("a")
				var aDials atomic.Int32
				obs["a"].dial = func(context.Context) (net.Conn, error) {
					aDials.Add(1)
					return nil, errors.New("dial timeout")
				}

				s.makeHooks("a", tt.network, tt.route).onFailure(tt.kind)
				synctest.Wait()

				uf, _ := userFailures(s, "a")
				require.Len(t, uf, 1, "every case is a recorded failure")
				if tt.confirms {
					assert.EqualValues(t, 1, aDials.Load(), "one confirmation probe")
					assert.Equal(t, "b", loadString(&s.stickyTag.tcp))
				} else {
					assert.Zero(t, aDials.Load(), "no confirmation probe")
					assert.Equal(t, "a", loadString(&s.stickyTag.tcp))
				}
			})
		})
	}
}

func TestTrySwitchAfterConfirmedFailure_ChoosesValidatedAlternate(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *MutableAutoSelect)
		want  string
	}{
		{"best validated", func(*MutableAutoSelect) {}, "b"},
		{"skips failed probe", func(s *MutableAutoSelect) { s.recordProbeOutcome("b", false, 0) }, "c"},
		{"skips gated", func(s *MutableAutoSelect) {
			s.access.Lock()
			s.historyForLocked("b").startRecoveryGate(time.Now())
			s.access.Unlock()
		}, "c"},
		{"none validated", func(s *MutableAutoSelect) {
			s.recordProbeOutcome("b", false, 0)
			s.recordProbeOutcome("c", false, 0)
		}, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := newConfirmationTestMUR(t, "a", "b", "c")
				tt.setup(s)
				s.trySwitchAfterConfirmedFailure("a", "")
				assert.Equal(t, tt.want, loadString(&s.stickyTag.tcp))
			})
		})
	}
}

func (h *localHistory) hasRecoveryGate() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.recoveryGateStartedAt.IsZero()
}

// blackholeConn accepts writes and never replies.
func blackholeConn() net.Conn {
	client, server := net.Pipe()
	go io.Copy(io.Discard, server)
	return client
}
