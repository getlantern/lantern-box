package meek

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks/socks5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// TestNewInbound_RequiresAuthToken verifies a meek inbound refuses to start as an
// open relay: an empty auth_token errors unless allow_unauthenticated is set. The
// check runs before any listener is bound, so a nil router/empty options is fine.
func TestNewInbound_RequiresAuthToken(t *testing.T) {
	_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "meek-in",
		option.MeekInboundOptions{})
	assert.Error(t, err, "empty auth_token without allow_unauthenticated must error")

	ib, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "meek-in",
		option.MeekInboundOptions{AllowUnauthenticated: true})
	// With the opt-in, the auth gate passes (any later error is unrelated to auth).
	if err != nil {
		assert.NotContains(t, err.Error(), "auth_token is required")
	} else {
		t.Cleanup(func() { _ = ib.Close() }) // it opened listeners — don't leak them
	}
}

// mockRouter implements adapter.ConnectionRouterEx with a controllable callback.
type mockRouter struct {
	onRoute func(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc)
}

func (m *mockRouter) RouteConnection(context.Context, net.Conn, adapter.InboundContext) error {
	return nil
}

func (m *mockRouter) RoutePacketConnection(context.Context, N.PacketConn, adapter.InboundContext) error {
	return nil
}

func (m *mockRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	if m.onRoute != nil {
		m.onRoute(ctx, conn, metadata, onClose)
	}
}

func (m *mockRouter) RoutePacketConnectionEx(context.Context, N.PacketConn, adapter.InboundContext, N.CloseHandlerFunc) {
}

// TestInbound_HandleSocks_RoutesConnect drives the SOCKS5 no-auth CONNECT the
// meek outbound sends (over a loopback pair, as the meek server does in prod)
// and checks the inbound terminates it, routes to the right destination, and
// pipes bytes — i.e. the microsocks-free data path works.
func TestInbound_HandleSocks_RoutesConnect(t *testing.T) {
	captured := make(chan adapter.InboundContext, 1)
	router := &mockRouter{
		onRoute: func(ctx context.Context, conn net.Conn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
			captured <- md
			go func() {
				defer onClose(nil)
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err == nil && n > 0 {
					_, _ = conn.Write(buf[:n]) // echo the app stream back
				}
			}()
		},
	}
	ib := &Inbound{
		Adapter: inbound.NewAdapter("meek", "meek-in"),
		ctx:     context.Background(),
		logger:  log.NewNOPFactory().Logger(),
		router:  router,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		s, aerr := ln.Accept()
		if aerr == nil {
			ib.handleSocks(s)
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))

	// 1. SOCKS5 no-auth method-select.
	require.NoError(t, socks5.WriteAuthRequest(c, socks5.AuthRequest{Methods: []byte{socks5.AuthTypeNotRequired}}))
	authReply := make([]byte, 2)
	require.NoError(t, readFull(c, authReply))
	require.Equal(t, socks5.Version, authReply[0])
	require.Equal(t, socks5.AuthTypeNotRequired, authReply[1])

	// 2. CONNECT, then consume the reply (header + bound addr + port).
	dst := M.ParseSocksaddr("93.184.216.34:443")
	require.NoError(t, socks5.WriteRequest(c, socks5.Request{Command: socks5.CommandConnect, Destination: dst}))
	require.NoError(t, readConnectReply(t, c))

	// 3. App bytes round-trip through the routed conn.
	_, err = c.Write([]byte("hello meek"))
	require.NoError(t, err)
	got := make([]byte, len("hello meek"))
	require.NoError(t, readFull(c, got))
	assert.Equal(t, "hello meek", string(got))

	// 4. The router saw the right destination + inbound identity (bounded wait so a
	// routing failure surfaces promptly instead of hanging to the test timeout).
	var md adapter.InboundContext
	select {
	case md = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the router to be invoked")
	}
	assert.Equal(t, "meek-in", md.Inbound)
	assert.Equal(t, "meek", md.InboundType)
	assert.Equal(t, "93.184.216.34", md.Destination.Addr.String())
	assert.Equal(t, uint16(443), md.Destination.Port)
}

// TestInbound_HandleSocks_RejectsNonConnect verifies a non-CONNECT command is
// refused (and the conn closed) rather than routed.
func TestInbound_HandleSocks_RejectsNonConnect(t *testing.T) {
	var routed atomic.Bool // written by the handler goroutine, read by the test
	ib := &Inbound{
		Adapter: inbound.NewAdapter("meek", "meek-in"),
		ctx:     context.Background(),
		logger:  log.NewNOPFactory().Logger(),
		router:  &mockRouter{onRoute: func(context.Context, net.Conn, adapter.InboundContext, N.CloseHandlerFunc) { routed.Store(true) }},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if s, aerr := ln.Accept(); aerr == nil {
			ib.handleSocks(s)
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))

	require.NoError(t, socks5.WriteAuthRequest(c, socks5.AuthRequest{Methods: []byte{socks5.AuthTypeNotRequired}}))
	authReply := make([]byte, 2)
	require.NoError(t, readFull(c, authReply))

	// CommandBind (0x02) is not supported by the inbound.
	require.NoError(t, socks5.WriteRequest(c, socks5.Request{Command: socks5.CommandBind, Destination: M.ParseSocksaddr("1.2.3.4:80")}))
	head := make([]byte, 4)
	require.NoError(t, readFull(c, head))
	assert.Equal(t, socks5.ReplyCodeUnsupported, head[1])
	assert.False(t, routed.Load(), "a non-CONNECT command must not be routed")
}

func readFull(r io.Reader, b []byte) error {
	_, err := io.ReadFull(r, b)
	return err
}

// readConnectReply consumes a SOCKS5 CONNECT reply: 4-byte header + bound addr +
// 2-byte port (mirrors the client's socks5ConnectSequenced).
func readConnectReply(t *testing.T, r io.Reader) error {
	t.Helper()
	head := make([]byte, 4)
	if err := readFull(r, head); err != nil {
		return err
	}
	require.Equal(t, socks5.Version, head[0])
	require.Equal(t, socks5.ReplyCodeSuccess, head[1])
	var addrLen int
	switch head[3] {
	case 0x01:
		addrLen = net.IPv4len
	case 0x04:
		addrLen = net.IPv6len
	case 0x03:
		lb := make([]byte, 1)
		if err := readFull(r, lb); err != nil {
			return err
		}
		addrLen = int(lb[0])
	}
	return readFull(r, make([]byte, addrLen+2))
}
