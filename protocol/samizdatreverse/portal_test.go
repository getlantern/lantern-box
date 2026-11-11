package samizdatreverse

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sblog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
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
	assert.Equal(t, addr, portalAddr(t, p))
	assert.Equal(t, "old", whoServes(p, tagA), "key rotation ended the live session")
	select {
	case <-old:
		t.Fatal("key rotation closed the live session")
	default:
	}

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
	select {
	case <-old:
	case <-time.After(5 * time.Second):
		t.Fatal("the new session did not replace the old one")
	}
	// The old identity held only that session, so it closes with it.
	require.Eventually(t, func() bool { return retiredCount(p) == 0 }, 5*time.Second, 20*time.Millisecond)
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

func TestPortalRebindKeepsPortAndSessions(t *testing.T) {
	k := newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{idA: tagA, idB: tagB})))
	addr := portalAddr(t, p)
	doneA := helper(t, addr, k, idA, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)

	p.mu.Lock()
	oldLn := p.ln
	p.mu.Unlock()
	require.NoError(t, p.Rebind())
	p.mu.Lock()
	newLn := p.ln
	p.mu.Unlock()
	assert.NotSame(t, oldLn, newLn, "Rebind kept the old listener")
	assert.Equal(t, addr, portalAddr(t, p), "Rebind moved the port")
	assert.EqualValues(t, 1, p.rebinds.Load())

	assert.Equal(t, "a", whoServes(p, tagA), "Rebind dropped a live session")
	select {
	case <-doneA:
		t.Fatal("Rebind closed a live session")
	default:
	}
	helper(t, addr, k, idB, "b")
	require.Eventually(t, func() bool { return whoServes(p, tagB) == "b" }, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, p.Close())
	require.NoError(t, p.Rebind(), "Rebind after Close is a no-op")
}

func TestPortalInterfaceUpdatedDebouncesRebind(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	p.rebindDelay = 200 * time.Millisecond
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	for range 5 {
		p.InterfaceUpdated(context.Background())
	}
	require.Eventually(t, func() bool { return p.rebinds.Load() == 1 }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	assert.EqualValues(t, 1, p.rebinds.Load(), "a burst of updates rebound more than once")

	assert.Equal(t, addr, portalAddr(t, p))
	helper(t, addr, k, id, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalInterfaceUpdatedWhenStopped(t *testing.T) {
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	p.rebindDelay = 10 * time.Millisecond
	p.InterfaceUpdated(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Nil(t, p.Addr())
	assert.EqualValues(t, 0, p.rebinds.Load())
}

func TestPortalFailedRebindRetriesOnNextUpdate(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	p.rebindDelay = 50 * time.Millisecond
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)
	port := p.Addr().(*net.TCPAddr).Port

	// Take the port in the gap between Rebind closing the old listener and
	// binding the new one, as another process could.
	var blocker net.Listener
	p.beforeListen = func(uint16) {
		p.beforeListen = nil
		var err error
		blocker, err = net.Listen("tcp", net.JoinHostPort("::", strconv.Itoa(port)))
		require.NoError(t, err)
	}
	err := p.Rebind()
	require.Error(t, err)
	assert.NotContains(t, addrFree(err).Error(), strconv.Itoa(port), "rebind error carries the port")
	assert.Nil(t, p.Addr(), "portal claims to serve after a failed rebind")
	_, ok := p.Session(tagA)
	assert.False(t, ok)

	require.NoError(t, blocker.Close())
	p.InterfaceUpdated(context.Background())
	require.Eventually(t, func() bool { return p.Addr() != nil }, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, addr, portalAddr(t, p), "portal came back on another port")
	helper(t, addr, k, id, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalRebindAfterServeExitKeepsPort(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	p.mu.Lock()
	ln, done := p.ln, p.serveDone
	p.mu.Unlock()
	require.NoError(t, ln.Close())
	<-done

	require.NoError(t, p.Rebind())
	assert.Equal(t, addr, portalAddr(t, p), "a port-0 portal moved to a new port")
	helper(t, addr, k, id, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)
}

func TestPortalEmptyConfigStopsRetries(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	require.NoError(t, p.Configure(k.config(0, nil)))
	require.NoError(t, p.Rebind())
	assert.Nil(t, p.Addr(), "Rebind restarted a portal configured with no pairings")
}

func TestPortalInvalidCertLeavesPortalServing(t *testing.T) {
	k := newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	port := freePort(t)
	pairings := map[[8]byte]string{idA: tagA, idB: tagB}
	require.NoError(t, p.Configure(k.config(port, pairings)))
	addr := portalAddr(t, p)
	doneA := helper(t, addr, k, idA, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)

	bad := k.config(port, pairings)
	bad.CertPEM = []byte("-----BEGIN CERTIFICATE-----\ngarbage\n-----END CERTIFICATE-----\n")
	require.Error(t, p.Configure(bad))
	short := k.config(port, pairings)
	short.PrivateKey = short.PrivateKey[:16]
	require.Error(t, p.Configure(short))

	assert.Equal(t, addr, portalAddr(t, p))
	assert.Equal(t, "a", whoServes(p, tagA), "existing session broken by a rejected config")
	select {
	case <-doneA:
		t.Fatal("a rejected config closed a live session")
	default:
	}
	helper(t, addr, k, idB, "b")
	require.Eventually(t, func() bool { return whoServes(p, tagB) == "b" }, 5*time.Second, 50*time.Millisecond)
}

// TestPortalConcurrentFirstSessions lines up two handlers for one tag on an
// empty slot, as the bridge's IPv6/IPv4 race can, with only one serving
// HTTP/2. Whichever is released first, the serving one must end up published
// and stay.
func TestPortalConcurrentFirstSessions(t *testing.T) {
	for _, servingFirst := range []bool{true, false} {
		name := "silent released first"
		if servingFirst {
			name = "serving released first"
		}
		t.Run(name, func(t *testing.T) {
			k := newTestKeys(t)
			id := newShortID(t)
			p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
			p.confirmTimeout = 500 * time.Millisecond
			arrived := make(chan chan struct{}, 2)
			p.beforePublish = func() {
				release := make(chan struct{})
				arrived <- release
				<-release
			}
			require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
			addr := portalAddr(t, p)

			waitArrival := func() chan struct{} {
				select {
				case r := <-arrived:
					return r
				case <-time.After(5 * time.Second):
					t.Fatal("handler did not reach publication")
					return nil
				}
			}
			silent := silentSession(t, addr, k, id)
			releaseSilent := waitArrival()
			serving := helper(t, addr, k, id, "serving")
			releaseServing := waitArrival()

			if servingFirst {
				close(releaseServing)
				require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 10*time.Millisecond)
				close(releaseSilent)
			} else {
				close(releaseSilent)
				require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 10*time.Millisecond)
				close(releaseServing)
			}

			select {
			case <-silent:
			case <-time.After(5 * time.Second):
				t.Fatal("the silent session was kept")
			}
			require.Eventually(t, func() bool { return whoServes(p, tagA) == "serving" }, 5*time.Second, 50*time.Millisecond)
			select {
			case <-serving:
				t.Fatal("the serving session was closed")
			default:
			}
		})
	}
}

type flushEach struct{ w http.ResponseWriter }

func (f flushEach) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	f.w.(http.Flusher).Flush()
	return n, err
}

// echoHelper is a helper whose HTTP/2 server allows maxStreams concurrent
// streams and echoes each CONNECT's body back until the portal closes it.
func echoHelper(t *testing.T, addr string, k testKeys, id [8]byte, maxStreams uint32) <-chan struct{} {
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
		(&http2.Server{MaxConcurrentStreams: maxStreams}).ServeConn(conn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			io.Copy(flushEach{w}, r.Body)
		})})
	}()
	return done
}

// openEcho opens a CONNECT stream on cc and returns a function that sends a
// message on it and checks the echo.
func openEcho(t *testing.T, cc *http2.ClientConn) func(msg string) error {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: "example.com:443"},
		Host:   "example.com:443",
		Header: http.Header{},
		Body:   pr,
	}
	resp, err := cc.RoundTrip(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return func(msg string) error {
		if _, err := pw.Write([]byte(msg)); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		got := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(resp.Body, buf)
			got <- err
		}()
		select {
		case err := <-got:
			if err != nil {
				return err
			}
		case <-time.After(3 * time.Second):
			return errors.New("echo timed out")
		}
		if string(buf) != msg {
			return errors.New("echo mismatch")
		}
		return nil
	}
}

func TestPortalSaturatedSessionIsNotGone(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	p.confirmTimeout = 500 * time.Millisecond
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	busy := echoHelper(t, addr, k, id, 2)
	var cc *http2.ClientConn
	require.Eventually(t, func() bool {
		var ok bool
		cc, ok = p.Session(tagA)
		return ok
	}, 5*time.Second, 20*time.Millisecond)
	streams := []func(string) error{openEcho(t, cc), openEcho(t, cc)}
	for _, echo := range streams {
		require.NoError(t, echo("before"))
	}
	require.Eventually(t, func() bool { return !cc.CanTakeNewRequest() }, 5*time.Second, 20*time.Millisecond,
		"session never reached its stream limit")

	silent := silentSession(t, addr, k, id)
	select {
	case <-silent:
	case <-time.After(5 * time.Second):
		t.Fatal("the silent candidate was kept")
	}
	select {
	case <-busy:
		t.Fatal("a silent candidate closed a busy session")
	default:
	}
	assert.False(t, cc.State().Closed)
	for _, echo := range streams {
		require.NoError(t, echo("after"), "a tunnel on the busy session broke")
	}

	helper(t, addr, k, id, "second")
	select {
	case <-busy:
	case <-time.After(5 * time.Second):
		t.Fatal("a confirmed candidate did not replace the busy session")
	}
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "second" }, 5*time.Second, 50*time.Millisecond)
}

// stalledSession authenticates with id and then never reads, so the portal's
// writes to it stall once the socket buffers fill.
func stalledSession(t *testing.T, addr string, k testKeys, id [8]byte) {
	t.Helper()
	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
}

func returnsWithin(d time.Duration, f func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func TestPortalStalledWriteDoesNotBlockPortal(t *testing.T) {
	k := newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	arrivals := make(chan struct{}, 8)
	p.beforePublish = func() { arrivals <- struct{}{} }
	pairings := map[[8]byte]string{idA: tagA, idB: tagB}
	require.NoError(t, p.Configure(k.config(0, pairings)))
	addr := portalAddr(t, p)

	helper(t, addr, k, idB, "b")
	<-arrivals
	require.Eventually(t, func() bool { return whoServes(p, tagB) == "b" }, 5*time.Second, 50*time.Millisecond)

	// Session A's helper stops reading. A request with a huge header makes
	// the portal's HTTP/2 client write until the socket is full, holding
	// its write lock, which State also takes.
	stalledSession(t, addr, k, idA)
	<-arrivals
	var ccA *http2.ClientConn
	require.Eventually(t, func() bool {
		var ok bool
		ccA, ok = p.Session(tagA)
		return ok
	}, 5*time.Second, 20*time.Millisecond)
	go func() {
		req := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Host: "example.com:443"},
			Host:   "example.com:443",
			Header: http.Header{"X-Big": {strings.Repeat("a", 8<<20)}},
		}
		if resp, err := ccA.RoundTrip(req); err == nil {
			resp.Body.Close()
		}
	}()
	require.Eventually(t, func() bool { return !returnsWithin(200*time.Millisecond, func() { ccA.State() }) },
		5*time.Second, 50*time.Millisecond, "the write to the stalled helper never blocked")

	// A second, healthy session for tag A arrives while A's write is stuck.
	helper(t, addr, k, idA, "a2")
	<-arrivals

	assert.True(t, returnsWithin(3*time.Second, func() { assert.Equal(t, "b", whoServes(p, tagB)) }),
		"Session for another tag blocked behind a stalled write")
	assert.True(t, returnsWithin(3*time.Second, func() { assert.NoError(t, p.Configure(k.config(0, pairings))) }),
		"Configure blocked behind a stalled write")

	// The replacement confirms itself without waiting on the stalled client,
	// takes the slot and restores traffic for tag A.
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a2" }, 10*time.Second, 50*time.Millisecond,
		"a healthy replacement for a stalled session never took over")
}

// retiredCount returns how many retired identities the portal holds that
// have not closed.
func retiredCount(p *Portal) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, id := range p.retired {
		if !id.isClosed() {
			n++
		}
	}
	return n
}

// withPrevious returns cfg with prev as its previous identity until until.
func withPrevious(cfg PortalConfig, prev testKeys, until time.Time) PortalConfig {
	cfg.Previous = &PreviousIdentity{PrivateKey: prev.priv, CertPEM: prev.cert, KeyPEM: prev.certKey, Until: until}
	return cfg
}

// authenticates reports whether a samizdat client with k and id completes a
// session with the portal at addr.
func authenticates(t *testing.T, addr string, k testKeys, id [8]byte) bool {
	t.Helper()
	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func TestPortalPreviousIdentityDuringGrace(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	idA, idB := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{idA: tagA, idB: tagB}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)

	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))))
	assert.Equal(t, addr, portalAddr(t, p))

	// Bridges pin the certificate of the identity they dial with, so each
	// must be served its own identity's certificate.
	newTestBridge(t, bridgeOptions(t, addr, k1, idA), &dialRouter{})
	newTestBridge(t, bridgeOptions(t, addr, k2, idB), &dialRouter{})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond,
		"a bridge with the previous identity did not connect during the grace period")
	require.Eventually(t, func() bool { _, ok := p.Session(tagB); return ok }, 5*time.Second, 20*time.Millisecond,
		"a bridge with the current identity did not connect")
}

func TestPortalPreviousIdentityRefusedAfterGrace(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	id, probe := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA, probe: tagB}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)

	until := time.Now().Add(time.Second)
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, until)))
	assert.True(t, authenticates(t, addr, k1, probe), "previous identity refused during the grace period")
	time.Sleep(time.Until(until))

	assert.False(t, authenticates(t, addr, k1, probe), "previous identity accepted after the grace period")
	opts := bridgeOptions(t, addr, k1, id)
	newTestBridge(t, opts, &dialRouter{})
	assert.Never(t, func() bool { _, ok := p.Session(tagA); return ok }, 2*time.Second, 50*time.Millisecond,
		"a bridge with the previous identity connected after the grace period")
	assert.True(t, authenticates(t, addr, k2, probe), "current identity refused")
	p.mu.Lock()
	assert.Nil(t, p.prev, "the expired identity is still previous")
	p.mu.Unlock()

	// Configuring it again after its time adds nothing.
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, until)))
	assert.False(t, authenticates(t, addr, k1, probe), "an expired previous identity was accepted")
}

func TestPortalDroppingPreviousRefusesItKeepsSessions(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	id, probe := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA, probe: tagB}
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))))
	addr := portalAddr(t, p)
	old := helper(t, addr, k1, id, "old")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "old" }, 5*time.Second, 50*time.Millisecond)

	// The inviter's routes are out with the new identity: it drops the old.
	require.NoError(t, p.Configure(k2.config(0, pairings)))
	assert.False(t, authenticates(t, addr, k1, probe), "a dropped previous identity was accepted")
	assert.Equal(t, "old", whoServes(p, tagA), "dropping the previous identity ended its session")
	select {
	case <-old:
		t.Fatal("dropping the previous identity closed its session")
	default:
	}
	assert.Equal(t, 1, retiredCount(p))
}

func TestPortalLiveSessionSurvivesKeyRotation(t *testing.T) {
	k1, k2, k3 := newTestKeys(t), newTestKeys(t), newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)
	newTestBridge(t, bridgeOptions(t, addr, k1, id), &dialRouter{})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)
	cc, _ := p.Session(tagA)
	o := newTestReverse(t, tagA, p)
	echo := tcpEcho(t)
	roundTrip := func() {
		t.Helper()
		conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr(echo))
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.Write([]byte("ping"))
		require.NoError(t, err)
		buf := make([]byte, 4)
		_, err = io.ReadFull(conn, buf)
		require.NoError(t, err)
		assert.Equal(t, "ping", string(buf))
	}
	roundTrip()

	// The first rotation keeps k1 as previous, the second retires it. Its
	// session stays the portal's throughout.
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))))
	got, ok := p.Session(tagA)
	require.True(t, ok, "a key-only Configure ended the live session")
	assert.Same(t, cc, got)
	roundTrip()

	require.NoError(t, p.Configure(withPrevious(k3.config(0, pairings), k2, time.Now().Add(time.Minute))))
	got, ok = p.Session(tagA)
	require.True(t, ok, "retiring the session's identity ended it")
	assert.Same(t, cc, got)
	roundTrip()
	assert.Equal(t, addr, portalAddr(t, p))
}

func TestPortalRevocationClosesSessionsOfEveryIdentity(t *testing.T) {
	k1, k2, k3 := newTestKeys(t), newTestKeys(t), newTestKeys(t)
	idA, idB, idC := newShortID(t), newShortID(t), newShortID(t)
	const tagC = "helper-cccccccccccc-r"
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{idA: tagA, idB: tagB, idC: tagC}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)
	doneA := helper(t, addr, k1, idA, "a")
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))))
	require.NoError(t, p.Configure(withPrevious(k3.config(0, pairings), k2, time.Now().Add(time.Minute))))
	// A is on the retired k1, B on the previous k2 and C on the current k3.
	doneB := helper(t, addr, k2, idB, "b")
	doneC := helper(t, addr, k3, idC, "c")
	for tag, name := range map[string]string{tagA: "a", tagB: "b", tagC: "c"} {
		require.Eventually(t, func() bool { return whoServes(p, tag) == name }, 5*time.Second, 50*time.Millisecond)
	}

	revoke := func(id [8]byte, tag string, done <-chan struct{}) {
		t.Helper()
		delete(pairings, id)
		require.NoError(t, p.Configure(withPrevious(k3.config(0, pairings), k2, time.Now().Add(time.Minute))))
		_, ok := p.Session(tag)
		assert.False(t, ok)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a revoked pairing's session stayed open")
		}
		assert.False(t, authenticates(t, addr, k1, id))
		assert.False(t, authenticates(t, addr, k2, id))
		assert.False(t, authenticates(t, addr, k3, id))
	}
	revoke(idA, tagA, doneA)
	revoke(idB, tagB, doneB)
	assert.Equal(t, "c", whoServes(p, tagC))
	revoke(idC, tagC, doneC)
	assert.Nil(t, p.Addr(), "no pairings left, yet the portal serves")
}

func TestPortalRejectsBadPreviousIdentity(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)
	done := helper(t, addr, k1, id, "a")
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "a" }, 5*time.Second, 50*time.Millisecond)

	until := time.Now().Add(time.Minute)
	same := withPrevious(k2.config(0, pairings), k2, until)
	require.Error(t, p.Configure(same), "a previous identity with the current key was accepted")
	bad := withPrevious(k2.config(0, pairings), k1, until)
	bad.Previous.CertPEM = []byte("-----BEGIN CERTIFICATE-----\ngarbage\n-----END CERTIFICATE-----\n")
	require.Error(t, p.Configure(bad))
	short := withPrevious(k2.config(0, pairings), k1, until)
	short.Previous.PrivateKey = short.Previous.PrivateKey[:16]
	require.Error(t, p.Configure(short))

	assert.Equal(t, "a", whoServes(p, tagA), "a rejected config broke the live session")
	select {
	case <-done:
		t.Fatal("a rejected config closed a live session")
	default:
	}
	assert.False(t, authenticates(t, addr, k2, id), "a rejected config was applied")
}

func TestPortalPreviousIdentityWithPortChange(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	id, probe := newShortID(t), newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA, probe: tagB}
	require.NoError(t, p.Configure(k1.config(0, pairings)))

	port := freePort(t)
	require.NoError(t, p.Configure(withPrevious(k2.config(port, pairings), k1, time.Now().Add(time.Minute))))
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	assert.True(t, authenticates(t, addr, k1, probe), "previous identity refused on the new port")
	assert.True(t, authenticates(t, addr, k2, probe), "current identity refused on the new port")
}

// TestPortalMasqueradeWithPreviousIdentity checks that a short ID neither
// identity accepts is masqueraded on the accepted conn itself, so the
// origin's close reaches the client at once, and that Close ends a
// masquerade still running.
func TestPortalMasqueradeWithPreviousIdentity(t *testing.T) {
	masq, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer masq.Close()
	var hold sync.Mutex
	var held []net.Conn
	closeFirst := true
	go func() {
		for {
			c, err := masq.Accept()
			if err != nil {
				return
			}
			hold.Lock()
			if closeFirst {
				closeFirst = false
				c.Close()
			} else {
				// Never answers or closes, like an origin waiting for more.
				held = append(held, c)
			}
			hold.Unlock()
		}
	}()
	t.Cleanup(func() {
		hold.Lock()
		defer hold.Unlock()
		for _, c := range held {
			c.Close()
		}
	})

	k1, k2 := newTestKeys(t), newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{MasqueradeDomain: "example.com", MasqueradeAddr: masq.Addr().String()})
	pairings := map[[8]byte]string{id: tagA}
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))))
	addr := portalAddr(t, p)

	start := time.Now()
	assert.False(t, authenticates(t, addr, k1, newShortID(t)))
	assert.Less(t, time.Since(start), 2*time.Second, "the origin's close did not reach the client")

	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k2.pub, ShortID: newShortID(t)})
	require.NoError(t, err)
	defer c.Close()
	dialed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := c.DialSession(ctx)
		if err == nil {
			conn.Close()
		}
		dialed <- err
	}()
	require.Eventually(t, func() bool { hold.Lock(); defer hold.Unlock(); return len(held) == 1 }, 5*time.Second, 20*time.Millisecond)
	require.True(t, returnsWithin(5*time.Second, func() { p.Close() }), "Close waited on a masquerade")
	select {
	case err := <-dialed:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close left a masqueraded conn open")
	}
}

func routingCount(p *Portal) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	return len(p.routing)
}

// TestPortalFailedHandshakesLeaveNothingTracked sends connections that never
// complete a ClientHello: closed at once, garbage, a truncated record, and
// silence until the hello timeout. None may stay in the routing map.
func TestPortalFailedHandshakesLeaveNothingTracked(t *testing.T) {
	old := helloTimeout
	helloTimeout = 200 * time.Millisecond
	t.Cleanup(func() { helloTimeout = old })

	k := newTestKeys(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{newShortID(t): tagA})))
	addr := portalAddr(t, p)

	payloads := [][]byte{
		nil,                              // EOF
		[]byte("GET / HTTP/1.1\r\n\r\n"), // not a handshake record
		{22, 3, 1, 0xff, 0xff},           // record too large
		{22, 3, 1, 0, 100, 1, 2, 3},      // truncated record, then EOF
	}
	for range 25 {
		for _, payload := range payloads {
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			require.NoError(t, err)
			if payload != nil {
				_, err = c.Write(payload)
				require.NoError(t, err)
			}
			c.Close()
		}
	}
	// Silent until the hello timeout closes them.
	var silent []net.Conn
	for range 10 {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		require.NoError(t, err)
		silent = append(silent, c)
	}
	t.Cleanup(func() {
		for _, c := range silent {
			c.Close()
		}
	})
	require.Eventually(t, func() bool { return routingCount(p) == 0 }, 5*time.Second, 20*time.Millisecond,
		"failed handshakes left connections tracked")
	for _, c := range silent {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := c.Read(make([]byte, 1))
		assert.ErrorIs(t, err, io.EOF, "a silent conn outlived the hello timeout")
	}
}

// TestPortalRetireKeepsAdmittedHandshake retires an identity after it has
// admitted a connection and before the connection reaches its server, when
// it holds no session. The handshake must complete and its session stay.
func TestPortalRetireKeepsAdmittedHandshake(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)

	admitted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.afterAdmit = func() {
		once.Do(func() {
			close(admitted)
			<-release
		})
	}
	dialed := make(chan *samizdat.Client, 1)
	conns := make(chan net.Conn, 1)
	go func() {
		c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k1.pub, ShortID: id})
		if err != nil {
			close(conns)
			return
		}
		dialed <- c
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := c.DialSession(ctx)
		if err != nil {
			close(conns)
			return
		}
		conns <- conn
	}()
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection was never admitted")
	}
	t.Cleanup(func() { (<-dialed).Close() })

	// k1 holds no session yet, only the admitted handshake.
	require.NoError(t, p.Configure(k2.config(0, pairings)))
	assert.Equal(t, 1, retiredCount(p), "the identity closed under an admitted handshake")
	close(release)

	conn, ok := <-conns
	require.True(t, ok, "the admitted handshake was cut off by retirement")
	t.Cleanup(func() { conn.Close() })
	go (&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Helper", "old")
		w.WriteHeader(http.StatusOK)
	})})
	require.Eventually(t, func() bool { return whoServes(p, tagA) == "old" }, 5*time.Second, 50*time.Millisecond)

	// Once that session ends, the retired identity closes.
	conn.Close()
	require.Eventually(t, func() bool { return retiredCount(p) == 0 }, 5*time.Second, 20*time.Millisecond)
	assert.False(t, authenticates(t, addr, k1, id), "a retired identity took a new session")
}

// TestPortalRejectsPreviousKeyClampedToCurrent uses a previous key that
// differs from the current one only in bits X25519 clamps away, so both
// make the same public key.
func TestPortalRejectsPreviousKeyClampedToCurrent(t *testing.T) {
	k1, k2 := newTestKeys(t), newTestKeys(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{newShortID(t): tagA}

	for _, flip := range []struct{ byte, mask int }{{0, 0x07}, {31, 0x80}, {31, 0x40}} {
		clamped := bytes.Clone(k2.priv)
		clamped[flip.byte] ^= byte(flip.mask)
		require.False(t, bytes.Equal(clamped, k2.priv))
		require.Equal(t, x25519Public(k2.priv), x25519Public(clamped), "the flipped bits are not clamped")

		prev := withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))
		prev.Previous.PrivateKey = clamped
		assert.Error(t, p.Configure(prev), "a previous key clamped to the current one was accepted")
	}
	require.NoError(t, p.Configure(withPrevious(k2.config(0, pairings), k1, time.Now().Add(time.Minute))))
}

// stalledHandshake sends a real client's authenticated ClientHello to addr
// and then nothing more. It returns the connection to the portal.
func stalledHandshake(t *testing.T, addr string, k testKeys, id [8]byte) net.Conn {
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { relay.Close() })
	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: relay.Addr().String(), ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if conn, err := c.DialSession(ctx); err == nil {
			conn.Close()
		}
	}()
	client, err := relay.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	record, err := readHelloRecord(client)
	require.NoError(t, err)
	portal, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { portal.Close() })
	_, err = portal.Write(record)
	require.NoError(t, err)
	return portal
}

// TestPortalStalledHandshakeDoesNotHoldRetiredIdentity admits an
// authenticated ClientHello whose sender then stalls, and retires its
// identity. The handshake timeout must close the connection, and the
// retired identity with it.
func TestPortalStalledHandshakeDoesNotHoldRetiredIdentity(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	k1, k2 := newTestKeys(t), newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	pairings := map[[8]byte]string{id: tagA}
	require.NoError(t, p.Configure(k1.config(0, pairings)))
	addr := portalAddr(t, p)

	admitted := make(chan struct{})
	var once sync.Once
	p.afterAdmit = func() { once.Do(func() { close(admitted) }) }
	conn := stalledHandshake(t, addr, k1, id)
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled handshake was never admitted")
	}

	require.NoError(t, p.Configure(k2.config(0, pairings)))
	require.Eventually(t, func() bool { return retiredCount(p) == 0 }, 5*time.Second, 20*time.Millisecond,
		"a stalled handshake held the retired identity open")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.Copy(io.Discard, conn)
	assert.NoError(t, err, "the portal did not close the stalled connection")
}
