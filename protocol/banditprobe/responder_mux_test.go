package banditprobe

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/mux"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// muxProbe runs a probe on a stream (a non-TCP conn) of a multiplex session
// whose socket is session, and returns the verdict it reported.
func muxProbe(t *testing.T, r *responder, rec *callbackRecorder, session *net.TCPConn, stream net.Conn, client net.Conn) string {
	t.Helper()
	ctx := mux.ContextWithSessionConn(context.Background(), session)
	errc := serveBlocked(t, r, ctx, stream, client)
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("probe on a multiplexed stream did not finish")
	}
	calls := rec.calls()
	require.Len(t, calls, 1)
	return calls[0].Get("verdict")
}

// drain reads and discards everything from c until it is closed, as a client
// reading the probe body does.
func drain(t *testing.T, c net.Conn) {
	t.Helper()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()
}

func TestServe_MuxStreamDeliveredFromSessionSocket(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, _ := tcpPair(t)
	var mu sync.Mutex
	var read []*net.TCPConn
	// Acks keep arriving, but other streams keep bytes outstanding, so
	// unacked never reaches 0.
	calls := uint64(0)
	states := func(*net.TCPConn) (sendState, error) {
		calls++
		return sendState{acked: 1000 + calls*100_000, unacked: 3000}, nil
	}
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, func(tc *net.TCPConn) (sendState, error) {
		mu.Lock()
		defer mu.Unlock()
		read = append(read, tc)
		return states(tc)
	})
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })
	drain(t, client)

	assert.Equal(t, "delivered", muxProbe(t, r, rec, session, stream, client))
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, read)
	for _, tc := range read {
		assert.Same(t, session, tc, "socket state must come from the session's socket")
	}
}

func TestServe_MuxStreamShortAckedSpanIsStalled(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, _ := tcpPair(t)
	// The socket drains, but far fewer bytes than the response were
	// acknowledged after the probe began, so the response wasn't delivered.
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 1000, unacked: 0},
		sendState{acked: 1100, unacked: 0},
	))
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })
	drain(t, client)

	assert.Equal(t, "stalled", muxProbe(t, r, rec, session, stream, client))
}

func TestServe_MuxStreamStallLeavesSessionSocketUsable(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, sessionPeer := tcpPair(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 1000, unacked: 5000},
	))
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })

	assert.Equal(t, "stalled", muxProbe(t, r, rec, session, newBlockingConn(stream), client))

	// The session's other streams must survive the probe's stall.
	_, err := session.Write([]byte("x"))
	require.NoError(t, err, "a stalled probe must not set a deadline on the shared socket")
	require.NoError(t, sessionPeer.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, err = sessionPeer.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "x", string(buf))
}

func TestServe_MuxStreamWriteIgnoringCloseIsAbandoned(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, _ := tcpPair(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 1000, unacked: 5000},
	))
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })

	start := time.Now()
	assert.Equal(t, "stalled", muxProbe(t, r, rec, session, newDeadlineIgnoringConn(t, stream, true), client))
	assert.Less(t, time.Since(start), time.Duration(r.cfg.maxWait)+3*writeAbortGrace)
}

func TestServe_NonMuxNonTCPConnStillUnknown(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, func(*net.TCPConn) (sendState, error) {
		t.Fatal("readState must not be called without a socket")
		return sendState{}, nil
	})
	sessionStream, _ := net.Pipe()
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close(); sessionStream.Close() })
	drain(t, client)

	// A session whose own conn isn't TCP (nested multiplexing) has no socket
	// to read either.
	ctx := mux.ContextWithSessionConn(context.Background(), sessionStream)
	errc := serveBlocked(t, r, ctx, stream, client)
	require.NoError(t, <-errc)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "unknown", calls[0].Get("verdict"))
}

func TestServe_MuxStreamAcksBeforeFirstPollCount(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, _ := tcpPair(t)
	// The whole response is acknowledged before the first poll after the
	// write starts; only a baseline taken before writing sees it.
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 1000, unacked: 0},
		sendState{acked: 1000 + 1<<20, unacked: 0},
	))
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })
	drain(t, client)

	assert.Equal(t, "delivered", muxProbe(t, r, rec, session, stream, client))
}

func TestServe_MuxStreamBytesQueuedAheadAreNotDelivery(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, _ := tcpPair(t)
	// Other streams had far more than a response's worth queued ahead of the
	// probe. Those get acknowledged and the flow freezes, so the
	// acknowledged span covers the response's length but never reaches the
	// probe's bytes.
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 1000, unacked: 500_000},
		sendState{acked: 101_000, unacked: 420_000},
	))
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })
	drain(t, client)

	assert.Equal(t, "stalled", muxProbe(t, r, rec, session, stream, client))
}

func TestServe_MuxStreamWithoutSocketStateLeavesSessionSocketUsable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		failOn int
	}{
		{"baseline read fails", 1},
		{"first poll fails", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := newCallbackRecorder(t)
			session, sessionPeer := tcpPair(t)
			var mu sync.Mutex
			calls := 0
			r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
				ReportStalled: true,
				MaxWait:       badoption.Duration(300 * time.Millisecond),
			}, func(*net.TCPConn) (sendState, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls >= tt.failOn {
					return sendState{}, errors.New("no socket state")
				}
				return sendState{acked: 1000, unacked: 5000}, nil
			})
			stream, client := net.Pipe()
			t.Cleanup(func() { client.Close(); stream.Close() })

			assert.Equal(t, "stalled", muxProbe(t, r, rec, session, newBlockingConn(stream), client))

			_, err := session.Write([]byte("x"))
			require.NoError(t, err, "a probe without socket state must not set a deadline on the shared socket")
			require.NoError(t, sessionPeer.SetReadDeadline(time.Now().Add(2*time.Second)))
			buf := make([]byte, 1)
			_, err = sessionPeer.Read(buf)
			require.NoError(t, err)
		})
	}
}

func TestServe_MuxStreamDrainedSessionIsDeliveredDespiteHighWatermark(t *testing.T) {
	rec := newCallbackRecorder(t)
	session, _ := tcpPair(t)
	// An ack between the two socket reads put the watermark at 151,000 when
	// only 141,000 bytes were written; the session then goes idle with
	// everything acknowledged.
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 1000, unacked: 0},
		sendState{acked: 1000, unacked: 0},
		sendState{acked: 101_000, unacked: 50_000},
		sendState{acked: 141_000, unacked: 0},
	))
	stream, client := net.Pipe()
	t.Cleanup(func() { client.Close(); stream.Close() })
	drain(t, client)

	assert.Equal(t, "delivered", muxProbe(t, r, rec, session, stream, client))
}
