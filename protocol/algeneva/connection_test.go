package algeneva

import (
	"context"
	"net"
	"testing"
	"time"

	alg "github.com/getlantern/algeneva"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"
)

func TestNewConnectionEx_HandshakeFailure(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	// Write invalid handshake data and close to simulate a bad client.
	go func() {
		server.Write([]byte("garbage\r\n\r\n"))
		server.Close()
	}()

	logger := log.StdLogger()
	in := &Inbound{
		Adapter: inbound.NewAdapter("algeneva", "test"),
		logger:  logger,
	}

	onCloseCalled := make(chan struct{})
	// Must not panic; must close the original conn.
	in.NewConnectionEx(context.Background(), client, adapter.InboundContext{}, func(err error) {
		close(onCloseCalled)
	})

	select {
	case <-onCloseCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("onClose was never called")
	}

	// The original conn should be closed.
	_, err := client.Read(make([]byte, 1))
	require.Error(t, err)
}

func TestE2E(t *testing.T) {
	destination := metadata.ParseSocksaddr("google.com:80")
	client, server := net.Pipe()
	strategy, _ := alg.NewHTTPStrategy("[HTTP:method:*]-insert{%0A:end:value:2}-|")
	logger := log.StdLogger()
	d := aDialer{
		Dialer:   &mockDialer{conn: server},
		strategy: strategy,
		logger:   logger,
	}
	in := &Inbound{logger: logger}
	go func() {
		_, err := in.newConnectionEx(context.Background(), client)
		require.NoError(t, err)
	}()

	_, err := d.DialContext(context.Background(), "tcp", destination)
	require.NoError(t, err)
}

type mockDialer struct {
	conn net.Conn
}

func (m *mockDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	return m.conn, nil
}
func (d *mockDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, nil
}
