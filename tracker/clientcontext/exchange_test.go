package clientcontext

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lAdapter "github.com/getlantern/lantern-box/adapter"
)

var testPayload = []byte(clientInfoPrefix + `{"DeviceID":"test-device"}`)

func setExchangeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := exchangeTimeout
	exchangeTimeout = d
	t.Cleanup(func() { exchangeTimeout = old })
}

type recordingPacketConn struct {
	written     []byte
	writtenAddr net.Addr
}

func (c *recordingPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, errors.ErrUnsupported
}

func (c *recordingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.written = slices.Clone(p)
	c.writtenAddr = addr
	return len(p), nil
}

func (c *recordingPacketConn) Close() error                     { return nil }
func (c *recordingPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *recordingPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingPacketConn) SetWriteDeadline(time.Time) error { return nil }

func TestExchangePacketAddr(t *testing.T) {
	ip := M.ParseSocksaddr("127.0.0.1:443")
	domain := M.Socksaddr{Fqdn: "example.com", Port: 443}
	tests := []struct {
		name string
		dest M.Socksaddr
		want net.Addr
	}{
		{name: "ip", dest: ip, want: ip.UDPAddr()},
		// Passed through so the outbound can carry the name to the server.
		{name: "domain", dest: domain, want: domain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &recordingPacketConn{}
			require.NoError(t, exchangePacket(context.Background(), conn, tt.dest, testPayload))
			assert.Equal(t, testPayload, conn.written)
			assert.Equal(t, tt.want, conn.writtenAddr)
		})
	}
}

var testExchanges = map[string]func(context.Context, net.Conn) error{
	"stream": func(ctx context.Context, conn net.Conn) error {
		return exchangeStream(ctx, conn, testPayload)
	},
	"packet": func(ctx context.Context, conn net.Conn) error {
		return exchangePacket(ctx, bufio.NewUnbindPacketConn(conn), M.ParseSocksaddr("example.com:443"), testPayload)
	},
}

// writeSignalConn signals each Write and ignores deadlines.
type writeSignalConn struct {
	net.Conn
	writes chan struct{}
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	c.writes <- struct{}{}
	return c.Conn.Write(p)
}

func (c *writeSignalConn) SetDeadline(time.Time) error {
	return errors.ErrUnsupported
}

// A write blocked on a peer that never reads is unblocked by cancellation, the
// context's deadline, or exchangeTimeout, even on a conn that ignores deadlines.
// A context canceled beforehand performs no I/O.
func TestExchangeCancel(t *testing.T) {
	tests := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		timeout time.Duration
		// cancelBlocked cancels ctx once the write is blocked.
		cancelBlocked bool
		wantErr       error
	}{
		{name: "before", ctx: canceledContext, wantErr: context.Canceled},
		{name: "cancel", ctx: cancelableContext, cancelBlocked: true, wantErr: context.Canceled},
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 100*time.Millisecond)
		}, wantErr: context.DeadlineExceeded},
		{name: "timeout", ctx: cancelableContext, timeout: 100 * time.Millisecond, wantErr: context.DeadlineExceeded},
	}
	for transport, exchange := range testExchanges {
		for _, tt := range tests {
			t.Run(transport+"/"+tt.name, func(t *testing.T) {
				setExchangeTimeout(t, cmp.Or(tt.timeout, time.Hour))
				ctx, cancel := tt.ctx()
				defer cancel()
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()
				conn := &writeSignalConn{Conn: client, writes: make(chan struct{}, 1)}

				done := make(chan error, 1)
				go func() { done <- exchange(ctx, conn) }()
				// A context done beforehand fails the exchange before any write.
				blocked := ctx.Err() == nil
				if blocked {
					select {
					case <-conn.writes:
					case <-time.After(time.Second):
						t.Fatal("exchange did not reach the blocked write")
					}
					if tt.cancelBlocked {
						cancel()
					}
				}
				select {
				case err := <-done:
					require.ErrorIs(t, err, tt.wantErr)
				case <-time.After(time.Second):
					t.Fatal("exchange did not stop")
				}
				if !blocked {
					require.Empty(t, conn.writes, "a canceled exchange must not perform I/O")
				}
			})
		}
	}
}

func cancelableContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func canceledContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx, cancel
}

// A cancellation or timeout after a successful exchange must not close the conn
// handed back to the caller.
func TestExchangeDetach(t *testing.T) {
	for name, exchange := range testExchanges {
		t.Run(name, func(t *testing.T) {
			setExchangeTimeout(t, 100*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))

			frames := make(chan string, 1)
			go func() {
				var payload [4096]byte
				n, err := server.Read(payload[:])
				if err != nil {
					return
				}
				frames <- string(payload[:n])
				io.Copy(server, server)
			}()

			require.NoError(t, exchange(ctx, client))
			assert.Equal(t, string(testPayload), <-frames)
			cancel()
			time.Sleep(200 * time.Millisecond)
			_, err := client.Write([]byte("data"))
			require.NoError(t, err)
			var data [4]byte
			_, err = io.ReadFull(client, data[:])
			require.NoError(t, err)
			assert.Equal(t, "data", string(data[:]))
		})
	}
}

type closeCountingConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// drainPipe reads and discards everything written to server.
func drainPipe(server net.Conn) {
	go io.Copy(io.Discard, server)
}

// Once the exchange starts it owns closing conn on failure, so a caller that
// only returns the error never closes conn a second time.
func TestExchangeClose(t *testing.T) {
	tests := []struct {
		name       string
		serve      func(server net.Conn)
		canceled   bool
		wantErr    bool
		wantCloses int32
	}{
		{name: "success", serve: drainPipe},
		{name: "write fails", serve: func(server net.Conn) { server.Close() }, wantErr: true, wantCloses: 1},
		{name: "timeout", serve: func(net.Conn) {}, wantErr: true, wantCloses: 1},
		{name: "canceled", serve: drainPipe, canceled: true, wantErr: true, wantCloses: 1},
	}
	for name, exchange := range testExchanges {
		for _, tt := range tests {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				setExchangeTimeout(t, 100*time.Millisecond)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tt.canceled {
					cancel()
				}
				client, server := net.Pipe()
				defer server.Close()
				tt.serve(server)

				conn := &closeCountingConn{Conn: client}
				err := exchange(ctx, conn)
				assert.Equal(t, tt.wantErr, err != nil, "err: %v", err)
				assert.Equal(t, tt.wantCloses, conn.closes.Load())
				client.Close()
			})
		}
	}
}

// recordingConn records each Write and reports a pending handshake until the
// first one, like a protocol conn that postpones its handshake. With failNext
// set, the next Write accepts only accept bytes and times out.
type recordingConn struct {
	net.Conn
	mu       sync.Mutex
	writes   []string
	failNext bool
	accept   int
}

func (c *recordingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext {
		c.failNext = false
		if c.accept > 0 {
			c.writes = append(c.writes, string(b[:c.accept]))
		}
		return c.accept, os.ErrDeadlineExceeded
	}
	c.writes = append(c.writes, string(b))
	return len(b), nil
}

func (c *recordingConn) NeedHandshake() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes) == 0
}

func (c *recordingConn) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.writes)
}

// The frame shares one inner write with the first payload. Probes find the
// pending handshake through common.Cast and the router through
// N.NeedHandshakeForWrite, including behind a group's TaggedConn, and copy loops
// must not bypass an unsent frame.
func TestFrameConn(t *testing.T) {
	tests := []struct {
		name  string
		first []byte
	}{
		{name: "payload", first: []byte("first")},
		// The router kicks a postponed handshake with an empty write when it has
		// no buffered payload, as for server-first protocols.
		{name: "empty", first: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &recordingConn{}
			conn := newFrameConn(inner, inner, testPayload)
			tagged := lAdapter.NewTaggedConn(conn, "member")
			early, ok := common.Cast[N.EarlyConn](conn)
			require.True(t, ok)
			assert.True(t, early.NeedHandshake())
			assert.True(t, N.NeedHandshakeForWrite(conn))
			assert.True(t, N.NeedHandshakeForWrite(tagged))
			assert.False(t, conn.WriterReplaceable())
			assert.True(t, conn.ReaderReplaceable())

			n, err := conn.Write(tt.first)
			require.NoError(t, err)
			assert.Equal(t, len(tt.first), n, "the reported count must exclude the frame")
			_, err = conn.Write([]byte("next"))
			require.NoError(t, err)

			assert.Equal(t, []string{string(testPayload) + string(tt.first), "next"}, inner.recorded())
			assert.False(t, N.NeedHandshakeForWrite(conn))
			assert.False(t, N.NeedHandshakeForWrite(tagged))
			assert.True(t, conn.WriterReplaceable())
		})
	}
}

func TestFrameConnConcurrent(t *testing.T) {
	inner := &recordingConn{}
	conn := newFrameConn(inner, inner, testPayload)

	var wg sync.WaitGroup
	for _, payload := range []string{"a", "b", "c", "d"} {
		wg.Go(func() {
			_, err := conn.Write([]byte(payload))
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	writes := inner.recorded()
	require.Len(t, writes, 4)
	assert.True(t, strings.HasPrefix(writes[0], string(testPayload)), "the frame must lead the first write")
	for _, w := range writes[1:] {
		assert.NotContains(t, w, string(testPayload), "the frame must be sent exactly once")
	}
}

// Probe conns share one payload, so the first write must not use its spare
// capacity.
func TestFrameConnSharedPayload(t *testing.T) {
	payload := make([]byte, len(testPayload), len(testPayload)+16)
	copy(payload, testPayload)
	inner := &recordingConn{}
	_, err := newFrameConn(inner, inner, payload).Write([]byte("data"))
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 16), payload[len(payload):cap(payload)])
}

// A first write that times out before the whole frame is out keeps the rest
// for the next write, since the router ignores a timed-out handshake kick and
// keeps relaying.
func TestFrameConnRetry(t *testing.T) {
	for _, accept := range []int{0, 5} {
		t.Run(fmt.Sprintf("%d-byte", accept), func(t *testing.T) {
			inner := &recordingConn{failNext: true, accept: accept}
			conn := newFrameConn(inner, inner, testPayload)

			n, err := conn.Write([]byte("first"))
			require.ErrorIs(t, err, os.ErrDeadlineExceeded)
			assert.Zero(t, n)
			assert.True(t, conn.NeedHandshake())
			assert.False(t, conn.WriterReplaceable())

			n, err = conn.Write([]byte("data"))
			require.NoError(t, err)
			assert.Equal(t, len("data"), n)
			assert.Equal(t, string(testPayload)+"data", strings.Join(inner.recorded(), ""))
			assert.True(t, conn.WriterReplaceable())
		})
	}
}
