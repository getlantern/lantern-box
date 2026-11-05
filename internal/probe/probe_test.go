package probe

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubOutbound struct {
	adapter.Outbound
	dials atomic.Int32
	dest  string
	dial  func(ctx context.Context) (net.Conn, error)
}

func (s *stubOutbound) Tag() string { return "stub" }

func (s *stubOutbound) DialContext(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
	s.dials.Add(1)
	s.dest = dest.String()
	return s.dial(ctx)
}

type trackedConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// dialerFor dials the server directly, standing in for an outbound that
// tunnels there.
func dialerFor(addr string, conns *[]*trackedConn) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		tracked := &trackedConn{Conn: c}
		*conns = append(*conns, tracked)
		return tracked, nil
	}
}

func TestRun_SuccessMeasuresDelayAndClosesConn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(addr, &conns)}

	delay, err := Run(context.Background(), out, srv.URL, 5*time.Second)

	require.NoError(t, err)
	assert.Positive(t, delay)
	assert.Less(t, delay, 5*time.Second)
	assert.Equal(t, addr, out.dest, "the URL's host and port are what gets dialed")
	require.Len(t, conns, 1)
	assert.True(t, conns[0].closed.Load(), "Run closes what it dials")
}

func TestRun_CompletedRequestSucceedsWhateverTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(strings.TrimPrefix(srv.URL, "http://"), &conns)}

	_, err := Run(context.Background(), out, srv.URL, 5*time.Second)

	assert.NoError(t, err, "a 500 still proves the request completed")
}

func TestRun_UnusableInputNeverDials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		probeURL string
		timeout  time.Duration
	}{
		{"empty URL", "", time.Second},
		{"unparsable URL", "http://[::1", time.Second},
		{"no scheme", "//probe.test/x", time.Second},
		{"unsupported scheme", "ftp://probe.test/x", time.Second},
		{"no host", "http:///x", time.Second},
		{"zero timeout", "http://probe.test/", 0},
		{"negative timeout", "http://probe.test/", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &stubOutbound{dial: func(context.Context) (net.Conn, error) {
				return nil, errors.New("must not dial")
			}}

			_, err := Run(context.Background(), out, tc.probeURL, tc.timeout)

			assert.ErrorIs(t, err, ErrUnusableInput)
			assert.Zero(t, out.dials.Load())
		})
	}
}

func TestRun_DialFailureWrapsTheOutboundsError(t *testing.T) {
	denied := errors.New("dial denied")
	out := &stubOutbound{dial: func(context.Context) (net.Conn, error) {
		return nil, denied
	}}

	delay, err := Run(context.Background(), out, "http://probe.test/", time.Second)

	assert.ErrorIs(t, err, denied)
	assert.NotErrorIs(t, err, ErrUnusableInput, "the dial happened; it failed")
	assert.Zero(t, delay)
}

func TestRun_TimeoutBoundsTheAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(strings.TrimPrefix(srv.URL, "http://"), &conns)}

	const timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), out, srv.URL, timeout)
	elapsed := time.Since(start)

	assert.ErrorIs(t, err, context.DeadlineExceeded, "a request that never completes is not a success")
	assert.Less(t, elapsed, 5*time.Second, "the per-probe timeout, not the caller's patience, ends it")
	require.Len(t, conns, 1)
	assert.True(t, conns[0].closed.Load())
}

func TestRun_CanceledContextIsAFailedProbe(t *testing.T) {
	out := &stubOutbound{dial: func(ctx context.Context) (net.Conn, error) {
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, out, "http://probe.test/", time.Second)

	assert.ErrorIs(t, err, context.Canceled)
}

func TestRun_ForwardsTraceparentFromQuery(t *testing.T) {
	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("traceparent")
	}))
	defer srv.Close()

	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(strings.TrimPrefix(srv.URL, "http://"), &conns)}

	_, err := Run(context.Background(), out, srv.URL+"?tp="+tp, 5*time.Second)

	require.NoError(t, err)
	assert.Equal(t, tp, <-got)
}

// firstWriteHandshakeConn blocks its first Write for cost, the way an outbound
// that runs its handshake on first use does.
type firstWriteHandshakeConn struct {
	net.Conn
	cost time.Duration
	done atomic.Bool
}

func (c *firstWriteHandshakeConn) Write(b []byte) (int, error) {
	if c.done.CompareAndSwap(false, true) {
		time.Sleep(c.cost)
	}
	return c.Conn.Write(b)
}

func TestRun_DelayExcludesConnectionSetupForEveryOutbound(t *testing.T) {
	const serverCost = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverCost)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	const dialCost = 400 * time.Millisecond

	slowDial := func(wrap func(net.Conn) net.Conn) func(context.Context) (net.Conn, error) {
		return func(ctx context.Context) (net.Conn, error) {
			time.Sleep(dialCost)
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			return wrap(c), nil
		}
	}

	for name, wrap := range map[string]func(net.Conn) net.Conn{
		"handshake on first write": func(c net.Conn) net.Conn {
			return &firstWriteHandshakeConn{Conn: c, cost: dialCost}
		},
		"handshake in dial": func(c net.Conn) net.Conn { return c },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			delay, err := Run(context.Background(), &stubOutbound{dial: slowDial(wrap)}, srv.URL, 5*time.Second)
			require.NoError(t, err)
			// The server can begin its sleep before the request's Write returns,
			// so the timed span may fall slightly short of serverCost.
			assert.GreaterOrEqual(t, delay, serverCost/2, "the request's round trip is timed")
			assert.Less(t, delay, dialCost, "connection setup is not")
		})
	}
}

type rootPool struct {
	adapter.CertificateStore
	pool *x509.CertPool
}

func (r rootPool) Pool() *x509.CertPool { return r.pool }

func TestRun_DelayIgnoresWritesAfterTheResponseBegins(t *testing.T) {
	t.Parallel()
	const serverCost = 300 * time.Millisecond
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverCost)
		w.Header().Set("Connection", "close")
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(strings.TrimPrefix(srv.URL, "https://"), &conns)}
	ctx := service.ContextWith[adapter.CertificateStore](context.Background(), rootPool{pool: pool})

	delay, err := Run(ctx, out, srv.URL, 5*time.Second)
	require.NoError(t, err)
	// Short of serverCost by however late the request's Write returns.
	assert.GreaterOrEqual(t, delay, serverCost/2, "a close_notify sent at body EOF does not restart the timer")
}

// slowReturnConn holds every Write for delay after the bytes are sent, so the
// response can arrive while the request's Write has yet to return.
type slowReturnConn struct {
	net.Conn
	delay time.Duration
}

func (c slowReturnConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	time.Sleep(c.delay)
	return n, err
}

func TestRun_RequestWriteStillInFlightAtTheResponseCounts(t *testing.T) {
	t.Parallel()
	const serverCost = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverCost)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	const writeReturn = 400 * time.Millisecond

	out := &stubOutbound{dial: func(ctx context.Context) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return slowReturnConn{Conn: c, delay: writeReturn}, nil
	}}

	delay, err := Run(context.Background(), out, srv.URL, 5*time.Second)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, delay, serverCost, "a write returning after the response does not hide the server's time")
	assert.Less(t, delay, writeReturn, "waiting for the write to return is not timed")
}

func TestWriteTimedConn_ResponseBeforeAnyWriteIsAnError(t *testing.T) {
	c := newWriteTimedConn(nil)
	c.freeze()
	_, err := c.lastWrite(context.Background())
	require.ErrorIs(t, err, errNoRequestWrite)
}

func TestRun_DerivesPortFromScheme(t *testing.T) {
	for _, tc := range []struct {
		name     string
		probeURL string
		want     string
	}{
		{"http defaults to 80", "http://probe.test/x", "probe.test:80"},
		{"https defaults to 443", "https://probe.test/x", "probe.test:443"},
		{"an explicit port wins", "http://probe.test:8080/x", "probe.test:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &stubOutbound{dial: func(context.Context) (net.Conn, error) {
				return nil, errors.New("dial denied")
			}}

			Run(context.Background(), out, tc.probeURL, time.Second)

			assert.Equal(t, tc.want, out.dest)
		})
	}
}
