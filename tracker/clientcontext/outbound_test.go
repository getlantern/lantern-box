package clientcontext

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/group"
)

// pipeOutbound dials net.Pipe conns, which have no pending handshake. With
// failWrites set, the far end is closed so the frame write fails.
type pipeOutbound struct {
	adapter.Outbound
	failWrites bool
	conns      []*closeCountingConn
}

func (o *pipeOutbound) Tag() string { return "lantern" }

func (o *pipeOutbound) dial() *closeCountingConn {
	client, server := net.Pipe()
	if o.failWrites {
		server.Close()
	} else {
		drainPipe(server)
	}
	conn := &closeCountingConn{Conn: client}
	o.conns = append(o.conns, conn)
	return conn
}

func (o *pipeOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return o.dial(), nil
}

func (o *pipeOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return bufio.NewUnbindPacketConn(o.dial()), nil
}

// lazyOutbound dials recordingConns, which postpone their handshake.
type lazyOutbound struct {
	adapter.Outbound
	conn *recordingConn
}

func (o *lazyOutbound) Tag() string { return "lantern" }

func (o *lazyOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	o.conn = &recordingConn{}
	return o.conn, nil
}

func newTestOutbound(inner adapter.Outbound) *outbound {
	return &outbound{
		Outbound: inner,
		injector: newTestInjector("lantern"),
	}
}

var testDest = M.ParseSocksaddr("example.com:443")

func TestOutboundDial(t *testing.T) {
	// The frame is written before DialContext returns and the conn is returned
	// unwrapped, so probes keep timing its dial.
	t.Run("handshaken", func(t *testing.T) {
		inner := &pipeOutbound{}
		conn, err := newTestOutbound(inner).DialContext(context.Background(), N.NetworkTCP, testDest)
		require.NoError(t, err)
		defer conn.Close()
		assert.Same(t, inner.conns[0], conn)
	})

	// Nothing is written until the caller's first write carries the frame.
	t.Run("pending handshake", func(t *testing.T) {
		inner := &lazyOutbound{}
		out := newTestOutbound(inner)
		conn, err := out.DialContext(context.Background(), N.NetworkTCP, testDest)
		require.NoError(t, err)
		require.IsType(t, &frameConn{}, conn)
		assert.Empty(t, inner.conn.recorded())

		_, err = conn.Write([]byte("hello"))
		require.NoError(t, err)
		payload := out.injector.encodePayload(context.Background())
		assert.Equal(t, []string{string(payload) + "hello"}, inner.conn.recorded())
	})
}

// A failed exchange closes its conn exactly once.
func TestOutboundCloseOnce(t *testing.T) {
	inner := &pipeOutbound{failWrites: true}
	out := newTestOutbound(inner)

	_, err := out.DialContext(context.Background(), N.NetworkTCP, testDest)
	assert.Error(t, err)
	_, err = out.DialContext(context.Background(), N.NetworkUDP, testDest)
	assert.Error(t, err)
	_, err = out.ListenPacket(context.Background(), testDest)
	assert.Error(t, err)

	require.Len(t, inner.conns, 3)
	for i, conn := range inner.conns {
		assert.Equal(t, int32(1), conn.closes.Load(), "conn %d", i)
	}
}

// Groups dial through member outbounds, which are wrapped themselves, so
// wrapping a group too would send two frames on one connection.
func TestCanWrap(t *testing.T) {
	t.Run("box outbounds", func(t *testing.T) {
		client := startTestServer(t).startClient(t, newTestInjector(), nil)
		for tag, want := range map[string]bool{"http-out": true, "socks-out": true, "selector": false} {
			out, ok := client.box.Outbound().Outbound(tag)
			require.True(t, ok, tag)
			_, wrapped := out.(*outbound)
			assert.Equal(t, want, wrapped, tag)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		fallback, err := group.NewFallback(context.Background(), nil, log.NewNOPFactory().NewLogger(""), "fallback",
			loption.FallbackOutboundOptions{Primary: "a", Fallback: "b"})
		require.NoError(t, err)
		assert.False(t, canWrapOutbound(fallback))
	})
}
