package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

type taggedOutbound struct{ adapter.Outbound }

func (taggedOutbound) Tag() string { return "direct" }

// routeWithin routes server through m's RoutedConnection, failing if it blocks.
func routeWithin(t *testing.T, m *clientcontext.Manager, server net.Conn, inbound string) net.Conn {
	t.Helper()
	done := make(chan net.Conn, 1)
	go func() {
		done <- m.RoutedConnection(context.Background(), server, adapter.InboundContext{Inbound: inbound}, nil, taggedOutbound{})
	}()
	select {
	case c := <-done:
		return c
	case <-time.After(time.Second):
		t.Fatal("the tracker read the connection")
		return nil
	}
}

// A direct exit stream carries the user's own traffic, with no client-info frame, so the
// tracker must not hold it reading for one. Every other inbound is still read.
func TestClientContextSkipsUnboundedExit(t *testing.T) {
	m := clientcontext.NewManager(clientContextBounds(), log.NewNOPFactory().Logger())

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	assert.Same(t, server, routeWithin(t, m, server, exit.InboundTag))

	server, client = net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() { _, _ = io.WriteString(client, "hi"); _ = client.Close() }()
	routed := routeWithin(t, m, server, "samizdat-in")
	assert.NotSame(t, server, routed, "other inbounds are read for client info")
	rest, err := io.ReadAll(routed)
	require.NoError(t, err)
	assert.Equal(t, "hi", string(rest))
}
