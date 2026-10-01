package banditprobe

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

func TestReadSendState_DeliveredOverRealSocket(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{BodySize: 64 * 1024}, readSendState)
	server, client := tcpPair(t)

	resp, body, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, 64*1024)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "delivered", calls[0].Get("verdict"))
}

// A client that stops reading with a tiny receive buffer closes its TCP window,
// so its kernel stops acknowledging: the same view a frozen flow gives the
// sender.
func TestReadSendState_StalledWhenPeerStopsAcknowledging(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		BodySize:      512 * 1024,
		ReportStalled: true,
		StallTimeout:  badoption.Duration(500 * time.Millisecond),
		MaxWait:       badoption.Duration(5 * time.Second),
	}, readSendState)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	dialer := net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
		var optErr error
		if err := c.Control(func(fd uintptr) {
			optErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048)
		}); err != nil {
			return err
		}
		return optErr
	}}
	client, err := dialer.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	server, err := l.Accept()
	require.NoError(t, err)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(client))

	errc := make(chan error, 1)
	go func() { errc <- r.serve(context.Background(), server) }()
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}

	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "stalled", calls[0].Get("verdict"))
}
