package samizdatreverse

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	sblog "github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/getlantern/lantern-box/option"
	samizdat "github.com/getlantern/samizdat"
)

const (
	tagA = "helper-aaaaaaaaaaaa-r"
	tagB = "helper-bbbbbbbbbbbb-r"
)

// testCertKeyPEM generates a self-signed ECDSA cert+key pair for testing.
func testCertKeyPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

type testKeys struct {
	priv, pub     []byte
	cert, certKey []byte
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	priv, pub, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)
	cert, key := testCertKeyPEM(t)
	return testKeys{priv: priv, pub: pub, cert: cert, certKey: key}
}

func newShortID(t *testing.T) [8]byte {
	t.Helper()
	id, err := samizdat.GenerateShortID()
	require.NoError(t, err)
	return id
}

func newTestPortal(t *testing.T, opts option.SamizdatPortalInboundOptions) *Portal {
	t.Helper()
	in, err := NewPortal(context.Background(), nil, sblog.NewNOPFactory().Logger(), "portal", opts)
	require.NoError(t, err)
	p := in.(*Portal)
	t.Cleanup(func() { p.Close() })
	return p
}

func (k testKeys) config(port uint16, pairings map[[8]byte]string) PortalConfig {
	return PortalConfig{ListenPort: port, PrivateKey: k.priv, CertPEM: k.cert, KeyPEM: k.certKey, Pairings: pairings}
}

func portalAddr(t *testing.T, p *Portal) string {
	t.Helper()
	addr := p.Addr()
	require.NotNil(t, addr)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.(*net.TCPAddr).Port))
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return uint16(port)
}

// helper dials the portal as a bridge would and serves HTTP/2 on the session,
// answering every CONNECT with an X-Helper header naming itself. The returned
// channel closes when the session ends.
func helper(t *testing.T, addr string, k testKeys, id [8]byte, name string) <-chan struct{} {
	t.Helper()
	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Helper", name)
			w.WriteHeader(http.StatusOK)
		})})
	}()
	return done
}

// whoServes opens a CONNECT on the portal's session for tag and returns the
// name of the helper that answered, or "" when there is no usable session.
func whoServes(p *Portal, tag string) string {
	cc, ok := p.Session(tag)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := (&http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: "example.com:443"},
		Host:   "example.com:443",
		Header: http.Header{},
	}).WithContext(ctx)
	resp, err := cc.RoundTrip(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	return resp.Header.Get("X-Helper")
}

func TestPortalSessionPerPairing(t *testing.T) {
	k := newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{idA: tagA, idB: tagB})))
	addr := portalAddr(t, p)

	helper(t, addr, k, idA, "a")
	helper(t, addr, k, idB, "b")

	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return whoServes(p, tagB) == "b" }, 5*time.Second, 50*time.Millisecond)
	_, ok := p.Session("helper-cccccccccccc-r")
	assert.False(t, ok)
}

func TestPortalNewerSessionReplacesOlder(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	first := helper(t, addr, k, id, "first")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "first" }, 5*time.Second, 50*time.Millisecond)

	helper(t, addr, k, id, "second")
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("portal did not close the older session")
	}
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "second" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalDropsRemovedPairing(t *testing.T) {
	k := newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{idA: tagA, idB: tagB})))
	addr := portalAddr(t, p)

	doneA := helper(t, addr, k, idA, "a")
	helper(t, addr, k, idB, "b")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return whoServes(p, tagB) == "b" }, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{idB: tagB})))
	assert.Equal(t, addr, portalAddr(t, p), "a pairings-only change must not rebind")
	_, ok := p.Session(tagA)
	assert.False(t, ok)
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("removed pairing's session stayed open")
	}
	assert.Equal(t, "b", whoServes(p, tagB))

	require.NoError(t, p.Configure(k.config(0, nil)))
	_, ok = p.Session(tagB)
	assert.False(t, ok)
	assert.Nil(t, p.Addr())
}

func TestPortalWrongShortIDGoesToMasquerade(t *testing.T) {
	masq, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer masq.Close()
	reached := make(chan struct{}, 1)
	go func() {
		for {
			c, err := masq.Accept()
			if err != nil {
				return
			}
			select {
			case reached <- struct{}{}:
			default:
			}
			c.Close()
		}
	}()

	k := newTestKeys(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{MasqueradeDomain: "example.com", MasqueradeAddr: masq.Addr().String()})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{newShortID(t): tagA})))

	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: portalAddr(t, p), ServerName: "localhost", PublicKey: k.pub, ShortID: newShortID(t)})
	require.NoError(t, err)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := c.DialSession(ctx); err == nil {
		conn.Close()
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("unknown short ID did not reach the masquerade")
	}
}

func TestPortalRebindsOnPortChange(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})

	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addrA := portalAddr(t, p)
	c, err := net.DialTimeout("tcp", addrA, 2*time.Second)
	require.NoError(t, err)
	c.Close()

	portB := freePort(t)
	require.NoError(t, p.Configure(k.config(portB, map[[8]byte]string{id: tagA})))
	_, err = net.DialTimeout("tcp", addrA, 2*time.Second)
	assert.Error(t, err, "old port still accepts")

	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(portB)))
	helper(t, addrB, k, id, "b")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "b" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalFailedRebindKeepsOldPort(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)
	helper(t, addr, k, id, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)

	busy, err := net.Listen("tcp", "[::]:0")
	require.NoError(t, err)
	defer busy.Close()
	busyPort := uint16(busy.Addr().(*net.TCPAddr).Port)

	require.Error(t, p.Configure(k.config(busyPort, map[[8]byte]string{id: tagA})))
	assert.Equal(t, addr, portalAddr(t, p))
	assert.Equal(t, "a", whoServes(p, tagA), "old session must survive a failed rebind")
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err, "old port stopped accepting")
	c.Close()
}

func TestPortalEpochShortIDsShareOneSlot(t *testing.T) {
	k := newTestKeys(t)
	prev, cur, next := newShortID(t), newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{prev: tagA, cur: tagA, next: tagA})))
	addr := portalAddr(t, p)

	first := helper(t, addr, k, prev, "prev")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "prev" }, 5*time.Second, 50*time.Millisecond)

	second := helper(t, addr, k, cur, "cur")
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("a session under another epoch's ID did not replace the older one")
	}
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "cur" }, 5*time.Second, 50*time.Millisecond)

	// The epoch rolls: prev leaves, a new next arrives. cur's session stays.
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{cur: tagA, next: tagA, newShortID(t): tagA})))
	assert.Equal(t, "cur", whoServes(p, tagA))
	select {
	case <-second:
		t.Fatal("rolling the epoch closed the current session")
	default:
	}
}

func TestPortalCloseEndsEverything(t *testing.T) {
	k := newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{idA: tagA, idB: tagB})))
	addr := portalAddr(t, p)
	doneA := helper(t, addr, k, idA, "a")
	doneB := helper(t, addr, k, idB, "b")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" && whoServes(p, tagB) == "b" }, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, p.Close())
	for _, done := range []<-chan struct{}{doneA, doneB} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close left a session open")
		}
	}
	_, ok := p.Session(tagA)
	assert.False(t, ok)
	assert.Nil(t, p.Addr())
	_, err := net.DialTimeout("tcp", addr, 2*time.Second)
	assert.Error(t, err, "listener still accepts after Close")
	assert.Error(t, p.Configure(k.config(0, map[[8]byte]string{idA: tagA})))
}

func TestPortalKeyRotationSamePort(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	port := freePort(t)
	require.NoError(t, p.Configure(k1.config(port, map[[8]byte]string{id: tagA})))
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	old := helper(t, addr, k1, id, "old")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "old" }, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, p.Configure(k2.config(port, map[[8]byte]string{id: tagA})))
	select {
	case <-old:
	case <-time.After(5 * time.Second):
		t.Fatal("key rotation left the old session open")
	}
	assert.Equal(t, addr, portalAddr(t, p))

	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k1.pub, ShortID: id})
	require.NoError(t, err)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if conn, err := c.DialSession(ctx); err == nil {
		conn.Close()
		t.Fatal("old key still authenticates")
	}

	helper(t, addr, k2, id, "new")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "new" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalRebuildsAfterServeExits(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	cfg := k.config(0, map[[8]byte]string{id: tagA})
	require.NoError(t, p.Configure(cfg))

	// Make Serve fail on its own, as an Accept error would.
	p.mu.Lock()
	ln, done := p.ln, p.serveDone
	p.mu.Unlock()
	require.NoError(t, ln.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not exit")
	}

	require.NoError(t, p.Configure(cfg))
	helper(t, portalAddr(t, p), k, id, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)
}

// silentSession authenticates with id but never serves HTTP/2, like the
// losing attempt of the bridge's dual-stack race. The returned channel
// closes when the portal closes the conn.
func silentSession(t *testing.T, addr string, k testKeys, id [8]byte) <-chan struct{} {
	t.Helper()
	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(io.Discard, conn)
	}()
	return done
}

func TestPortalUnconfirmedSessionDoesNotReplaceLive(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	p.confirmTimeout = 500 * time.Millisecond
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	first := helper(t, addr, k, id, "first")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "first" }, 5*time.Second, 50*time.Millisecond)

	silent := silentSession(t, addr, k, id)
	select {
	case <-silent:
	case <-time.After(5 * time.Second):
		t.Fatal("portal kept a session that never answered its ping")
	}
	select {
	case <-first:
		t.Fatal("an unconfirmed session closed the live one")
	default:
	}
	assert.Equal(t, "first", whoServes(p, tagA))

	helper(t, addr, k, id, "second")
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("a session that serves HTTP/2 did not replace the older one")
	}
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "second" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalCloseDuringConfirmPing(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	p.confirmTimeout = time.Minute
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	helper(t, addr, k, id, "first")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "first" }, 5*time.Second, 50*time.Millisecond)
	silent := silentSession(t, addr, k, id)
	time.Sleep(200 * time.Millisecond) // let its handler reach the ping

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		p.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a handler blocked in its confirm ping")
	}
	<-silent
}
