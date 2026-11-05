package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"syscall"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/control"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"
	samizdat "github.com/getlantern/samizdat"
)

const reverseTag = "helper-0123456789ab-r"

// TestSamizdatReverseThroughHelper runs reverse direct mode end to end on
// loopback with real boxes: the inviter box takes SOCKS and routes it to a
// reverse outbound, whose streams travel down the session the helper box's
// bridge dialed into the inviter's portal, and exit through the helper's
// direct outbound.
func TestSamizdatReverseThroughHelper(t *testing.T) {
	priv, pub, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)
	shortID, err := samizdat.GenerateShortID()
	require.NoError(t, err)
	certPEM, keyPEM, pin := selfSignedCert(t)

	socksPort := freePort(t)
	portalPort := freePort(t)

	inviterCtx := box.BaseContext()
	inviterOpts := twiddleOptions(inviterCtx, t, "testdata/samizdat_reverse_inviter.json", map[string]string{
		"SOCKS_PORT": strconv.Itoa(int(socksPort)),
	})
	inviter, err := sbox.New(sbox.Options{Context: inviterCtx, Options: inviterOpts})
	require.NoError(t, err)
	require.NoError(t, inviter.Start())
	t.Cleanup(func() { inviter.Close() })

	// A desktop box binds the portal's listener to the default physical
	// interface, as it must to keep the portal off a TUN, and loopback
	// cannot reach a socket bound there. Stand in for a mobile VPN's
	// protect hook, which leaves a loopback socket alone. The portal looks
	// the network manager up when it binds, so swapping it now is enough.
	service.MustRegister[adapter.NetworkManager](inviterCtx, loopbackNetwork{inviter.Network()})

	in, ok := inviter.Inbound().Get("samizdat-portal")
	require.True(t, ok, "the inviter box has no portal inbound")
	portal, ok := in.(interface {
		Configure(samizdatreverse.PortalConfig) error
	})
	require.True(t, ok, "the portal inbound cannot be configured")
	require.NoError(t, portal.Configure(samizdatreverse.PortalConfig{
		ListenPort: portalPort,
		PrivateKey: priv,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		Pairings:   map[[8]byte]string{shortID: reverseTag},
	}))

	helperSubs := map[string]string{
		"PORTAL_PORT": strconv.Itoa(int(portalPort)),
		"PUBLIC_KEY":  hex.EncodeToString(pub),
		"SHORT_ID":    hex.EncodeToString(shortID[:]),
		"CERT_PIN":    hex.EncodeToString(pin[:]),
	}
	// Each box gets its own base context: boxes built from one share a
	// service registry, so the inviter's reverse outbound would look its
	// portal up in whichever box registered its inbound manager last.
	startHelper := func() *sbox.Box {
		ctx := box.BaseContext()
		opts := twiddleOptions(ctx, t, "testdata/samizdat_reverse_helper.json", helperSubs)
		b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
		require.NoError(t, err)
		require.NoError(t, b.Start())
		return b
	}
	helperBox := startHelper()
	t.Cleanup(func() { helperBox.Close() })

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "exited from the helper")
	}))
	t.Cleanup(origin.Close)

	client := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", socksPort), socks.Version5, "", "")
	get := func(timeout time.Duration) (string, error) {
		hc := &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return client.DialContext(ctx, network, M.ParseSocksaddr(addr))
				},
			},
		}
		resp, err := hc.Get(origin.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}
	requireThrough := func(within time.Duration, msg string) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			body, err := get(5 * time.Second)
			if err == nil {
				require.Equal(t, "exited from the helper", body)
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %v", msg, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// The bridge dials the portal once its box starts, so the first request
	// may beat the session.
	requireThrough(10*time.Second, "no request made it through the helper")

	// With the helper gone the reverse outbound has no session and refuses
	// at once, rather than leaving the request to hang.
	require.NoError(t, helperBox.Close())
	start := time.Now()
	_, err = get(5 * time.Second)
	elapsed := time.Since(start)
	require.Error(t, err, "a request succeeded with the helper box closed")
	require.Less(t, elapsed, time.Second, "a request with no helper took %v to fail", elapsed)

	// A restarted helper box runs a new bridge, which dials at once rather
	// than waiting out the old one's backoff.
	helperBox = startHelper()
	requireThrough(10*time.Second, "requests did not recover after the helper box restarted")
}

// selfSignedCert returns a self-signed ECDSA cert and key as the portal
// serves them, and the SHA-256 of the cert's SPKI that bridges pin.
func selfSignedCert(t *testing.T) (certPEM, keyPEM []byte, pin [32]byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	pin, err = samizdat.SPKISHA256(der)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pin
}

// loopbackNetwork is a network manager whose protect hook binds nothing.
type loopbackNetwork struct {
	adapter.NetworkManager
}

func (loopbackNetwork) ProtectFunc() control.Func {
	return func(network, address string, conn syscall.RawConn) error { return nil }
}
