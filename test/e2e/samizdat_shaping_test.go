package e2e

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	samizdat "github.com/getlantern/samizdat"
)

// samizdatPair returns a samizdat inbound and a matching outbound, as
// probeThrough takes them, each with extra JSON fields appended (empty for
// none).
func samizdatPair(t *testing.T, inboundExtra, outboundExtra string) (string, string) {
	t.Helper()
	priv, pub, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)
	sid, err := samizdat.GenerateShortID()
	require.NoError(t, err)
	certPEM, keyPEM, _ := selfSignedCert(t)
	cert, err := json.Marshal(string(certPEM))
	require.NoError(t, err)
	key, err := json.Marshal(string(keyPEM))
	require.NoError(t, err)
	inbound := `{"type":"samizdat","tag":"samizdat-in","listen":"127.0.0.1","listen_port":%d,` +
		`"private_key":"` + hex.EncodeToString(priv) + `","short_ids":["` + hex.EncodeToString(sid[:]) + `"],` +
		`"cert_pem":` + string(cert) + `,"key_pem":` + string(key) + inboundExtra + `}`
	outbound := `{"type":"samizdat","tag":"samizdat-out","server":"127.0.0.1","server_port":%d,` +
		`"public_key":"` + hex.EncodeToString(pub) + `","short_id":"` + hex.EncodeToString(sid[:]) + `",` +
		`"server_name":"localhost","disable_tcp_fragmentation":true` + outboundExtra + `}`
	return inbound, outbound
}

// Shaping is on by default at both ends.
func TestSamizdatShapingByDefault(t *testing.T) {
	in, out := samizdatPair(t, "", "")
	probeThrough(t, in, out)
}

// Today's clients, which don't shape, keep working against an inbound that
// accepts shaping.
func TestSamizdatUnshapedClientThroughShapingInbound(t *testing.T) {
	in, out := samizdatPair(t, "", `,"disable_shaping":true`)
	probeThrough(t, in, out)
}

// With shaping off at both ends the route works; were the outbound's
// disable_shaping ignored, its client would shape and fail against the
// inbound below.
func TestSamizdatShapingDisabledAtBothEnds(t *testing.T) {
	in, out := samizdatPair(t, `,"disable_shaping":true`, `,"disable_shaping":true`)
	probeThrough(t, in, out)
}

// A shaping client fails against an inbound with disable_shaping, which
// shows the inbound's option reaches the server.
func TestSamizdatShapingClientFailsOnInboundWithout(t *testing.T) {
	in, out := samizdatPair(t, `,"disable_shaping":true`, "")
	require.Error(t, probeThroughErr(t, in, out))
}
