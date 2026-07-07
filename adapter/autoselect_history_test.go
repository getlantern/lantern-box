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
