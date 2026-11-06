package samizdatreverse

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	sblog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/getlantern/lantern-box/option"
	samizdat "github.com/getlantern/samizdat"
)

func newTestReverse(t *testing.T, tag string, src sessionSource) *Outbound {
	t.Helper()
	out, err := NewReverseOutbound(context.Background(), nil, sblog.NewNOPFactory().Logger(), tag, option.SamizdatReverseOutboundOptions{Portal: "portal"})
	require.NoError(t, err)
	o := out.(*Outbound)
	if src != nil {
		o.source = func() (sessionSource, bool) { return src, true }
	}
	t.Cleanup(func() { o.Close() })
	return o
}

// handlerConn is a test net.Conn over a CONNECT handler's request body and
// response writer.
type handlerConn struct {
	net.Conn // nil, only to satisfy the interface
	r        io.Reader
	w        http.ResponseWriter
}

func (c *handlerConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *handlerConn) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.w.(http.Flusher).Flush()
	return n, err
}
func (c *handlerConn) Close() error                     { return nil }
func (c *handlerConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *handlerConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *handlerConn) SetDeadline(time.Time) error      { return nil }
func (c *handlerConn) SetReadDeadline(time.Time) error  { return nil }
func (c *handlerConn) SetWriteDeadline(time.Time) error { return nil }

// relayHandler answers CONNECT the way an inviter's bridge would: it dials
// r.Host and relays, speaking UoT when r.Host is the UoT magic address.
func relayHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		stream := &handlerConn{r: r.Body, w: w}
		if M.ParseSocksaddr(r.Host).Fqdn == uot.MagicAddress {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			req, err := uot.ReadRequest(stream)
			if err != nil {
				return
			}
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				return
			}
			defer pc.Close()
			uc := uot.NewConn(stream, *req)
			go func() {
				buf := make([]byte, 2048)
				for {
					n, _, err := pc.ReadFrom(buf)
					if err != nil {
						return
					}
					if _, err := uc.WriteTo(buf[:n], req.Destination.UDPAddr()); err != nil {
						return
					}
				}
			}()
			buf := make([]byte, 2048)
			for {
				n, _, err := uc.ReadFrom(buf)
				if err != nil {
					return
				}
				if _, err := pc.WriteTo(buf[:n], req.Destination.UDPAddr()); err != nil {
					return
				}
			}
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer up.Close()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		go func() {
			io.Copy(up, r.Body)
			up.(*net.TCPConn).CloseWrite()
		}()
		io.Copy(stream, up)
	})
}

// relayHelper dials the portal as an inviter's bridge would and serves
// relayHandler on the session.
func relayHelper(t *testing.T, addr string, k testKeys, id [8]byte, h http.Handler) {
	t.Helper()
	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	go (&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: h})
}

func portalWithSession(t *testing.T, h http.Handler) *Portal {
	t.Helper()
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	relayHelper(t, portalAddr(t, p), k, id, h)
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)
	return p
}

func tcpEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func udpEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestReverseFailsFastWithoutSession(t *testing.T) {
	dest := M.ParseSocksaddr("127.0.0.1:9")
	for name, o := range map[string]*Outbound{
		"no portal":  newTestReverse(t, tagA, nil),
		"no session": newTestReverse(t, tagB, newTestPortal(t, option.SamizdatPortalInboundOptions{})),
	} {
		t.Run(name, func(t *testing.T) {
			for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
				start := time.Now()
				conn, err := o.DialContext(context.Background(), network, dest)
				assert.ErrorIs(t, err, ErrNoSession)
				assert.Nil(t, conn)
				assert.Less(t, time.Since(start), 50*time.Millisecond)
			}
			start := time.Now()
			_, err := o.ListenPacket(context.Background(), dest)
			assert.ErrorIs(t, err, ErrNoSession)
			assert.Less(t, time.Since(start), 50*time.Millisecond)
		})
	}
}

func TestReverseDialsThroughSession(t *testing.T) {
	p := portalWithSession(t, relayHandler(http.StatusOK))
	o := newTestReverse(t, tagA, p)
	echo := tcpEcho(t)

	conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr(echo))
	require.NoError(t, err)
	defer conn.Close()

	// Longer than the header timeout would allow if it leaked into the stream.
	for _, msg := range []string{"hello", "again"} {
		_, err = conn.Write([]byte(msg))
		require.NoError(t, err)
		buf := make([]byte, len(msg))
		_, err = io.ReadFull(conn, buf)
		require.NoError(t, err)
		assert.Equal(t, msg, string(buf))
	}

	// Half-close reaches the far end: the echo sees EOF and ends the stream.
	require.NoError(t, N.CloseWrite(conn))
	n, err := conn.Read(make([]byte, 1))
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)
}

func TestReverseStreamOutlivesHeaderTimeout(t *testing.T) {
	old := connectTimeout
	connectTimeout = 200 * time.Millisecond
	defer func() { connectTimeout = old }()

	p := portalWithSession(t, relayHandler(http.StatusOK))
	o := newTestReverse(t, tagA, p)
	conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr(tcpEcho(t)))
	require.NoError(t, err)
	defer conn.Close()

	time.Sleep(3 * connectTimeout)
	_, err = conn.Write([]byte("late"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "late", string(buf))
}

func TestReverseDialCanceledByCaller(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)
	p := portalWithSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stall }))
	o := newTestReverse(t, tagA, p)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := o.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr("127.0.0.1:9"))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestReverseNon200HidesDestination(t *testing.T) {
	p := portalWithSession(t, relayHandler(http.StatusBadGateway))
	o := newTestReverse(t, tagA, p)

	conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("10.11.12.13:4567"))
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.NotContains(t, err.Error(), "10.11.12.13")
	assert.NotContains(t, err.Error(), "4567")
	assert.False(t, strings.Contains(err.Error(), tagA))
}

func TestReverseUDPOverTCP(t *testing.T) {
	p := portalWithSession(t, relayHandler(http.StatusOK))
	o := newTestReverse(t, tagA, p)
	echo := udpEcho(t)

	conn, err := o.DialContext(context.Background(), N.NetworkUDP, M.ParseSocksaddr(echo))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf[:n]))

	pc, err := o.ListenPacket(context.Background(), M.ParseSocksaddr(echo))
	require.NoError(t, err)
	defer pc.Close()
	udpAddr, err := net.ResolveUDPAddr("udp", echo)
	require.NoError(t, err)
	_, err = pc.WriteTo([]byte("pong"), udpAddr)
	require.NoError(t, err)
	n, _, err = pc.ReadFrom(buf)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(buf[:n]))
}

func TestReverseHeaderTimeout(t *testing.T) {
	old := connectTimeout
	connectTimeout = 200 * time.Millisecond
	defer func() { connectTimeout = old }()

	stall := make(chan struct{})
	defer close(stall)
	p := portalWithSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stall }))
	o := newTestReverse(t, tagA, p)

	start := time.Now()
	_, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("127.0.0.1:9"))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// rawTCP digs the TCP conn out from under a samizdat session conn.
func rawTCP(t *testing.T, c net.Conn) *net.TCPConn {
	t.Helper()
	for {
		if tc, ok := c.(*net.TCPConn); ok {
			return tc
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		require.True(t, ok, "cannot reach the TCP conn under %T", c)
		c = u.NetConn()
	}
}

func TestReverseSessionResetHidesAddresses(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	got := make(chan struct{}, 1)
	go (&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
		<-r.Context().Done()
	})})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	o := newTestReverse(t, tagA, p)
	errc := make(chan error, 1)
	go func() {
		_, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("127.0.0.1:9"))
		errc <- err
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("CONNECT never reached the helper")
	}

	// Reset the session's TCP connection while the CONNECT waits for its
	// response headers.
	tc := rawTCP(t, conn)
	localPort := strconv.Itoa(tc.LocalAddr().(*net.TCPAddr).Port)
	require.NoError(t, tc.SetLinger(0))
	require.NoError(t, tc.Close())

	select {
	case err = <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("CONNECT did not fail after the session reset")
	}
	require.Error(t, err)
	t.Logf("CONNECT error: %v", err)
	_, portalPort, _ := net.SplitHostPort(addr)
	for _, leak := range []string{"127.0.0.1", "[::", portalPort, localPort} {
		assert.NotContains(t, err.Error(), leak, "CONNECT error names a session endpoint")
	}
}

func TestReverseStreamResetHidesAddresses(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	addr := portalAddr(t, p)

	c, err := samizdat.NewClient(samizdat.ClientConfig{ServerAddr: addr, ServerName: "localhost", PublicKey: k.pub, ShortID: id})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialSession(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	go (&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	o := newTestReverse(t, tagA, p)
	stream, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("127.0.0.1:9"))
	require.NoError(t, err)
	defer stream.Close()

	tc := rawTCP(t, conn)
	localPort := strconv.Itoa(tc.LocalAddr().(*net.TCPAddr).Port)
	require.NoError(t, tc.SetLinger(0))
	require.NoError(t, tc.Close())

	readErr := make(chan error, 1)
	go func() {
		_, err := stream.Read(make([]byte, 1))
		readErr <- err
	}()
	select {
	case err = <-readErr:
	case <-time.After(10 * time.Second):
		t.Fatal("stream read did not fail after the session reset")
	}
	require.Error(t, err)
	t.Logf("stream read error: %v", err)
	_, portalPort, _ := net.SplitHostPort(addr)
	for _, leak := range []string{"127.0.0.1", "[::", portalPort, localPort} {
		assert.NotContains(t, err.Error(), leak, "stream error names a session endpoint")
	}
}
