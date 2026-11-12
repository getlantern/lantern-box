package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"

	box "github.com/getlantern/lantern-box"

	"github.com/stretchr/testify/require"

	samizdat "github.com/getlantern/samizdat"
)

// samizdatPair returns a samizdat inbound and a matching outbound, as
// probeThrough takes them, each with extra JSON fields appended (empty for
// none).
func samizdatPair(t *testing.T, inboundExtra, outboundExtra string) (string, string) {
	t.Helper()
	in, out, _ := samizdatPairPin(t, inboundExtra, outboundExtra)
	return in, out
}

// samizdatPairPin is samizdatPair also returning the hex pin of the
// inbound's certificate.
func samizdatPairPin(t *testing.T, inboundExtra, outboundExtra string) (string, string, string) {
	t.Helper()
	priv, pub, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)
	sid, err := samizdat.GenerateShortID()
	require.NoError(t, err)
	certPEM, keyPEM, pin := selfSignedCert(t)
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
	return inbound, outbound, hex.EncodeToString(pin[:])
}

// Shaping is on by default at both ends.
func TestSamizdatShapingByDefault(t *testing.T) {
	in, out := samizdatPair(t, "", "")
	probeThrough(t, in, out)
}

// Today's clients, which send version 1 auth and don't shape, keep working
// against an inbound that accepts version 2 and shaping.
func TestSamizdatUnshapedClientThroughShapingInbound(t *testing.T) {
	in, out := samizdatPair(t, "", `,"disable_shaping":true,"legacy_auth":true`)
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

// sentSessionID runs a client box whose samizdat outbound, with extra JSON
// fields appended, dials a listener that only records the first TLS record,
// and returns that ClientHello's SessionID and the outbound's short ID.
func sentSessionID(t *testing.T, outboundExtra string) ([]byte, [8]byte) {
	t.Helper()
	_, pub, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)
	sid, err := samizdat.GenerateShortID()
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		rec := make([]byte, int(hdr[3])<<8|int(hdr[4]))
		if _, err := io.ReadFull(c, rec); err != nil {
			return
		}
		got <- rec
	}()

	ctx := box.BaseContext()
	clientPort := freePort(t)
	cli := fmt.Sprintf(`{"log":{"level":"warn"},"inbounds":[{"type":"mixed","tag":"in","listen":"127.0.0.1","listen_port":%d}],`+
		`"outbounds":[{"type":"samizdat","tag":"samizdat-out","server":"127.0.0.1","server_port":%d,`+
		`"public_key":"%s","short_id":"%s","server_name":"localhost","disable_tcp_fragmentation":true%s}]}`,
		clientPort, ln.Addr().(*net.TCPAddr).Port, hex.EncodeToString(pub), hex.EncodeToString(sid[:]), outboundExtra)
	co, err := sjson.UnmarshalExtendedContext[option.Options](ctx, []byte(cli))
	require.NoError(t, err)
	cb, err := sbox.New(sbox.Options{Context: ctx, Options: co})
	require.NoError(t, err)
	require.NoError(t, cb.Start())
	defer cb.Close()

	client := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return client.DialContext(ctx, network, M.ParseSocksaddr(addr))
		},
	}}
	go hc.Get("http://example.invalid/")
	select {
	case rec := <-got:
		sessionID, err := samizdat.ExtractSessionID(rec)
		require.NoError(t, err)
		return sessionID, sid
	case <-time.After(10 * time.Second):
		t.Fatal("the outbound sent no ClientHello")
	}
	return nil, sid
}

// legacy_auth reaches the client: with it, the SessionID is version 1's,
// which starts with the short ID; without it, version 2's, which doesn't.
func TestSamizdatLegacyAuthOption(t *testing.T) {
	v1, sid := sentSessionID(t, `,"legacy_auth":true`)
	require.True(t, bytes.HasPrefix(v1, sid[:]), "legacy_auth sent a SessionID that isn't version 1's")
	v2, sid := sentSessionID(t, "")
	require.False(t, bytes.HasPrefix(v2, sid[:]), "the default sent version 1's SessionID")
}

// cert_spki_sha256 reaches the client: pinned to the inbound's certificate
// the route works, pinned to another it fails the handshake.
func TestSamizdatCertPinOption(t *testing.T) {
	in, out, pin := samizdatPairPin(t, "", "")
	probeThrough(t, in, strings.Replace(out, `}`, `,"cert_spki_sha256":"`+pin+`"}`, 1))

	in, out, _ = samizdatPairPin(t, "", "")
	wrong := strings.Repeat("ab", 32)
	require.Error(t, probeThroughErr(t, in, strings.Replace(out, `}`, `,"cert_spki_sha256":"`+wrong+`"}`, 1)))
}
