package samizdat

import (
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	samizdat "github.com/getlantern/samizdat"
)

// newTestInbound returns an Inbound around a samizdat server that accepts one
// short ID, and that ID. It does not listen.
func newTestInbound(t *testing.T) (*Inbound, [8]byte) {
	t.Helper()
	cert, key := testCertKeyPEM(t)
	priv, err := hex.DecodeString(testPrivKeyHex(t))
	require.NoError(t, err)
	var base [8]byte
	b, err := hex.DecodeString(testShortIDHex(t))
	require.NoError(t, err)
	copy(base[:], b)
	srv, err := samizdat.NewServer(samizdat.ServerConfig{
		PrivateKey: priv,
		ShortIDs:   [][8]byte{base},
		CertPEM:    []byte(cert),
		KeyPEM:     []byte(key),
		Handler:    func(context.Context, net.Conn, string) {},
	})
	require.NoError(t, err)
	return &Inbound{server: srv}, base
}

func TestInbound_UpdateShortIDs(t *testing.T) {
	ib, base := newTestInbound(t)
	baseHex := hex.EncodeToString(base[:])
	paired := testShortIDHex(t)

	require.NoError(t, ib.UpdateShortIDs([]string{baseHex, paired}))
	got := ib.server.ShortIDs()
	require.Len(t, got, 2)
	assert.Equal(t, base, got[0])
	assert.Equal(t, paired, hex.EncodeToString(got[1][:]))

	require.NoError(t, ib.UpdateShortIDs([]string{baseHex}))
	assert.Equal(t, [][8]byte{base}, ib.server.ShortIDs())
}

func TestInbound_UpdateShortIDsRejectsBadSets(t *testing.T) {
	ib, base := newTestInbound(t)
	for name, ids := range map[string][]string{
		"nil":       nil,
		"empty":     {},
		"not hex":   {"zz" + strings.Repeat("00", 7)},
		"too short": {"0011"},
		"one bad":   {hex.EncodeToString(base[:]), "bad"},
	} {
		err := ib.UpdateShortIDs(ids)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "short_ids", name)
		assert.Equal(t, [][8]byte{base}, ib.server.ShortIDs(), "%s: a rejected update leaves the set unchanged", name)
	}
}
