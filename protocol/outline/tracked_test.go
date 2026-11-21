package outline

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
	"golang.getoutline.org/sdk/transport"
	"golang.getoutline.org/sdk/x/configurl"
)

// trackingRegistry returns the outbound's config URL registry over a sing-box
// dialer that tracks its connections, as every dialer inside a running box
// does, and the tracker.
func trackingRegistry(t *testing.T) (configurl.Registry, *route.ConnectionManager) {
	t.Helper()
	manager := route.NewConnectionManager(logger.NOP())
	ctx := service.ContextWith[adapter.ConnectionManager](context.Background(), manager)
	d, err := dialer.New(ctx, O.DialerOptions{}, false)
	require.NoError(t, err)
	return newRegistry(&outboundDialer{dialer: d, packetDialer: d}), manager
}

// TestStrategiesOverTrackedConnections dials each kind of TLS strategy the
// server sends through a tracking dialer. Before the stream dialer looked
// through the tracker's wrapper, every dial failed, so the hedge never went
// proxyless.
func TestStrategiesOverTrackedConnections(t *testing.T) {
	registry, manager := trackingRegistry(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	received := make(chan string, 1)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(c)
			c.Close()
			received <- string(b)
		}
	}()

	for _, config := range []string{"", "split:1", "split:2,20*5", "disorder:1|split:200"} {
		sd, err := configurl.New[transport.StreamDialer](context.Background(), registry, config)
		require.NoError(t, err, config)
		conn, err := sd.DialStream(context.Background(), l.Addr().String())
		require.NoError(t, err, config)
		require.Equal(t, 1, manager.Count(), "%q: the connection is tracked", config)
		_, err = conn.Write([]byte("hello, " + config))
		require.NoError(t, err, config)
		require.NoError(t, conn.CloseWrite(), config)
		select {
		case got := <-received:
			require.Equal(t, "hello, "+config, got)
		case <-time.After(5 * time.Second):
			t.Fatalf("%q: nothing arrived", config)
		}
		require.NoError(t, conn.Close(), config)
		require.Zero(t, manager.Count(), "%q: closing the stream stops tracking it", config)
	}
}

// TestPacketDialerOverTrackedConnections round-trips a datagram through the
// outbound's packet dialer, which UDP DNS resolvers use, over a tracking
// dialer.
func TestPacketDialerOverTrackedConnections(t *testing.T) {
	registry, manager := trackingRegistry(t)
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(buf[:n], from)
		}
	}()

	pd, err := configurl.New[transport.PacketDialer](context.Background(), registry, "")
	require.NoError(t, err)
	conn, err := pd.DialPacket(context.Background(), echo.LocalAddr().String())
	require.NoError(t, err)
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf[:n]))
	require.NoError(t, conn.Close())
	require.Zero(t, manager.Count())
}
