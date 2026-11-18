package group

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lConst "github.com/getlantern/lantern-box/constant"
)

// newLastResortMUR builds a group with a regular member "a" and an unbounded
// member "ub".
func newLastResortMUR(t *testing.T) (*MutableAutoSelect, map[string]*mockOutbound) {
	t.Helper()
	s, obs := newTestMUR(t, "a", "ub")
	obs["ub"].typeName = lConst.TypeUnbounded
	return s, obs
}

func TestSelectOutbound_LastResortOnlyWhenNothingHealthy(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(s *MutableAutoSelect)
		wantTag string
	}{
		{
			name: "clean regular member beats a faster last resort",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 900)
				recordSuccess(s, "ub", 50)
			},
			wantTag: "a",
		},
		{
			name: "soft-demoted regular member still beats the last resort",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 900)
				recordSuccess(s, "ub", 50)
				addUserFailureN(s, "a", int(s.hist.softFailLimit))
			},
			wantTag: "a",
		},
		{
			name: "last resort carries traffic once every regular member is hard-demoted",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				recordSuccess(s, "ub", 5000)
				addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
			},
			wantTag: "ub",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newLastResortMUR(t)
			tt.setup(s)
			got, _, err := s.selectOutbound("tcp", "")
			require.NoError(t, err)
			assert.Equal(t, tt.wantTag, got.Tag())
		})
	}
}

func TestRank_LastResortTierSitsBetweenSoftAndHard(t *testing.T) {
	s, obs := newTestMUR(t, "soft", "ub", "hard")
	obs["ub"].typeName = lConst.TypeUnbounded
	recordSuccess(s, "soft", 100)
	recordSuccess(s, "ub", 100)
	recordSuccess(s, "hard", 100)
	addUserFailureN(s, "soft", int(s.hist.softFailLimit))
	addUserFailureN(s, "hard", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now())
	s.access.Unlock()

	var order []string
	levels := map[string]demoteLevel{}
	for _, c := range ranked {
		order = append(order, c.tag)
		levels[c.tag] = c.demote
	}
	assert.Equal(t, []string{"soft", "ub", "hard"}, order)
	assert.Equal(t, demoteLastResort, levels["ub"])
}

func TestRank_HardDemotedLastResortStaysHard(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "ub", 100)
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now())
	s.access.Unlock()
	for _, c := range ranked {
		if c.tag == "ub" {
			assert.Equal(t, demoteHard, c.demote, "a failing last resort must not outrank failing regular members")
		}
	}
}

func TestSelectOutbound_LeavesLastResortWhenARegularMemberRecovers(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "a", 100)
	recordSuccess(s, "ub", 100)
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	got, _, err := s.selectOutbound("tcp", "")
	require.NoError(t, err)
	require.Equal(t, "ub", got.Tag())

	// "a" recovers, and is slower than the sticky last resort by more than
	// the switch tolerance would normally require to move.
	s.access.Lock()
	delete(s.histories, "a")
	s.access.Unlock()
	recordSuccess(s, "a", 100)

	got, _, err = s.selectOutbound("tcp", "")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Tag())
}

func TestSelectOutboundExcluding_NoFastFailoverToLastResortWhileRegularHealthy(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(s *MutableAutoSelect)
		wantErr bool
	}{
		{
			name: "failed member is still clean",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				addUserFailureN(s, "a", 1)
			},
			wantErr: true,
		},
		{
			name: "failed member is soft-demoted",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				addUserFailureN(s, "a", int(s.hist.softFailLimit))
			},
			wantErr: true,
		},
		{
			name: "failed member is hard-demoted",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, ubHard := range []bool{false, true} {
				s, _ := newLastResortMUR(t)
				recordSuccess(s, "ub", 4000)
				if ubHard {
					addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))
				}
				tt.setup(s)
				got, _, err := s.selectOutbound("tcp", "a")
				if tt.wantErr {
					require.Errorf(t, err, "one failed dial on a healthy regular member must not route through the last resort (ub hard=%v)", ubHard)
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, "ub", got.Tag())
			}
		})
	}
}

func TestSelectOutboundExcluding_PrefersAnotherRegularMemberOverLastResort(t *testing.T) {
	s, obs := newTestMUR(t, "a", "b", "ub")
	obs["ub"].typeName = lConst.TypeUnbounded
	recordSuccess(s, "a", 100)
	recordSuccess(s, "b", 300)
	recordSuccess(s, "ub", 50)
	got, _, err := s.selectOutbound("tcp", "a")
	require.NoError(t, err)
	assert.Equal(t, "b", got.Tag())
}

func TestRank_LastResortGetsNoSwitchPenaltyBoost(t *testing.T) {
	// The boost would double the hard limit for a member much faster than
	// its best alternative; a last resort must be held to the normal limit.
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "ub", 100)
	recordSuccess(s, "a", 990)
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now())
	s.access.Unlock()
	for _, c := range ranked {
		if c.tag == "ub" {
			assert.Equal(t, demoteHard, c.demote)
		}
	}
}

func TestRank_HardRegularOutranksHardLastResort(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "a", 900)
	recordSuccess(s, "ub", 50)
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))

	got, _, err := s.selectOutbound("tcp", "")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Tag(), "when everything is hard-demoted, a regular member beats a faster last resort")
}

func TestSelectOutbound_LeavesHardStickyLastResortForHardRegular(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "a", 100)
	recordSuccess(s, "ub", 100)
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	got, _, err := s.selectOutbound("tcp", "")
	require.NoError(t, err)
	require.Equal(t, "ub", got.Tag())

	// The sticky last resort fails too; both are now hard, with equal delays
	// that the tolerance rule alone would keep sticky.
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))
	got, _, err = s.selectOutbound("tcp", "")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Tag())
}
