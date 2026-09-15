package testing

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// TestDialContext_DialsDestinationDirectly checks that the outbound dials the
// destination it is handed and returns a working connection. Literal TUN escape
// depends on the box network manager's interface binding and is an integration
// property, not exercised here.
func TestDialContext_DialsDestinationDirectly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		conn.Write(buf)
	}()

	out, err := NewOutbound(context.Background(), nil, log.StdLogger(), "test", option.TestingOutboundOptions{})
	require.NoError(t, err)

	conn, err := out.DialContext(context.Background(), "tcp", M.ParseSocksaddr(ln.Addr().String()))
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)

	got := make([]byte, 4)
	_, err = conn.Read(got)
	require.NoError(t, err)
	require.Equal(t, "ping", string(got))
}
