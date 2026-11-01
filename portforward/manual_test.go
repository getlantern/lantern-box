package portforward

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ParseManualPort accepts 1..65535 verbatim and rejects everything else
// with an error so callers can log + fall through to UPnP discovery
// rather than register a non-listening port with lantern-cloud.
func TestParseManualPort(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint16
		wantErr bool
	}{
		{"valid mid-range", "5698", 5698, false},
		{"valid low boundary", "1", 1, false},
		{"valid high boundary", "65535", 65535, false},
		{"empty", "", 0, true},
		{"non-numeric", "abc", 0, true},
		{"zero", "0", 0, true},
		{"negative", "-5", 0, true},
		{"above uint16", "65536", 0, true},
		{"way above uint16", "99999", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseManualPort(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Equal(t, uint16(0), got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ManualForwarder satisfies the portForwarder contract: MapPort returns
// a Mapping with external==internal port, Protocol="TCP" matching the
// UPnP forwarder, and the "manual" method tag. UnmapPort and
// StartRenewal are no-ops, ExternalIP returns "" so the server
// substitutes the IP it observed on the register call.
func TestManualForwarder(t *testing.T) {
	f := NewManualForwarder(5698)

	m, err := f.MapPort(context.Background(), 30001, "ignored")
	require.NoError(t, err)
	assert.Equal(t, uint16(5698), m.ExternalPort)
	assert.Equal(t, uint16(5698), m.InternalPort, "external==internal — user mapped them themselves")
	assert.Equal(t, "TCP", m.Protocol, "Protocol matches UPnP forwarder's hard-coded value")
	assert.Equal(t, "manual", m.Method)

	require.NoError(t, f.UnmapPort(context.Background()), "UnmapPort is a no-op for manual forwarders")

	// StartRenewal must not panic or block.
	f.StartRenewal(context.Background())

	ip, err := f.ExternalIP(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ip, "empty IP signals server to use observed source address")
}

// MapPort defensively rejects a zero-port forwarder so a caller that
// somehow gets one (bypassing pickManualForwarder's range check) can
// fall through to UPnP rather than register a non-listening port.
func TestManualForwarder_RejectsZeroPort(t *testing.T) {
	f := NewManualForwarder(0)
	m, err := f.MapPort(context.Background(), 30001, "ignored")
	assert.Nil(t, m)
	assert.Error(t, err)
}
