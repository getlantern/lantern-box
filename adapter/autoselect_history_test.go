package adapter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserFailure_UnmarshalJSON_ObjectForm(t *testing.T) {
	at := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	data, err := json.Marshal(UserFailure{At: at, Kind: UserFailureStall})
	require.NoError(t, err)

	var got UserFailure
	require.NoError(t, json.Unmarshal(data, &got))
	assert.True(t, got.At.Equal(at))
	assert.Equal(t, UserFailureStall, got.Kind)
}

func TestUserFailure_UnmarshalJSON_LegacyTimestamp(t *testing.T) {
	// Snapshots persisted before the kind was tracked stored each failure
	// as a bare RFC3339 timestamp string. Those must still load, tagged as
	// unknown, so an in-place upgrade doesn't fail to read history.
	at := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	legacy, err := json.Marshal(at)
	require.NoError(t, err)

	var got UserFailure
	require.NoError(t, json.Unmarshal(legacy, &got))
	assert.True(t, got.At.Equal(at))
	assert.Equal(t, UserFailureUnknown, got.Kind)
}

func TestTagHistory_UnmarshalJSON_LegacyUserFailures(t *testing.T) {
	// A whole TagHistory persisted with the legacy []time.Time window must
	// decode without error.
	legacy := `{"user_failures":["2026-07-06T12:00:00Z","2026-07-06T12:01:00Z"]}`
	var got TagHistory
	require.NoError(t, json.Unmarshal([]byte(legacy), &got))
	require.Len(t, got.UserFailures, 2)
	for _, f := range got.UserFailures {
		assert.Equal(t, UserFailureUnknown, f.Kind)
	}
}

func TestTagHistory_JSONRoundTripNetworks(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := TagHistory{
		LastSuccessDelayMs: 40,
		UserFailures:       []UserFailure{{At: at, Kind: UserFailureDial, Network: "tcp"}},
		UpdatedAt:          at,
		Networks: map[uint64]*NetworkHistory{
			0:          {LastSuccessDelayMs: 90, UpdatedAt: at},
			0xfeedface: {LastSuccessDelayMs: 40, UserFailures: []UserFailure{{At: at, Kind: UserFailureDial, Network: "tcp"}}, HardDemoted: true, UpdatedAt: at},
		},
	}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	var out TagHistory
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, in, out)
}

func TestTagHistory_UnmarshalJSON_WithoutNetworks(t *testing.T) {
	var h TagHistory
	require.NoError(t, json.Unmarshal([]byte(`{"last_success_delay_ms":12,"user_failures":[{"at":"2026-01-02T03:04:05Z","kind":"stall"}],"updated_at":"2026-01-02T03:04:05Z"}`), &h))
	assert.Nil(t, h.Networks)
	require.Len(t, h.UserFailures, 1)
	assert.Empty(t, h.UserFailures[0].Network, "a failure recorded before network attribution has no network")
}

func TestCloneTagHistory_DoesNotAliasNetworks(t *testing.T) {
	at := time.Now()
	orig := &TagHistory{
		UserFailures: []UserFailure{{At: at, Network: "tcp"}},
		Networks: map[uint64]*NetworkHistory{
			1: {LastSuccessDelayMs: 10, UserFailures: []UserFailure{{At: at, Network: "udp"}}},
		},
	}
	c := cloneTagHistory(orig)
	c.UserFailures[0].Network = "x"
	c.Networks[1].LastSuccessDelayMs = 99
	c.Networks[1].UserFailures[0].Network = "x"
	c.Networks[2] = &NetworkHistory{}

	assert.Equal(t, "tcp", orig.UserFailures[0].Network)
	assert.Equal(t, uint32(10), orig.Networks[1].LastSuccessDelayMs)
	assert.Equal(t, "udp", orig.Networks[1].UserFailures[0].Network)
	assert.Len(t, orig.Networks, 1)
}

func TestUserFailure_UnmarshalJSON_LegacyTimestampClearsNetwork(t *testing.T) {
	u := UserFailure{Kind: UserFailureDial, Network: "tcp"}
	require.NoError(t, json.Unmarshal([]byte(`"2026-01-02T03:04:05Z"`), &u))
	assert.Equal(t, UserFailureUnknown, u.Kind)
	assert.Empty(t, u.Network, "a legacy failure has no network")
}
