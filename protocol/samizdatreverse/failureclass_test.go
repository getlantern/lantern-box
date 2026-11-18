package samizdatreverse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	C "github.com/sagernet/sing-box/constant"
	sbdns "github.com/sagernet/sing-box/dns"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route/rule"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/constant"
)

func dialErr(errno syscall.Errno) error {
	return &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
		Err:  os.NewSyscallError("connect", errno),
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestFailureClass(t *testing.T) {
	rejected := (&rule.RuleActionReject{Method: C.RuleActionRejectMethodDefault, NoDrop: true}).Error(context.Background())
	require.Error(t, rejected)

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"rejected", rejected, "rejected"},
		{"wrapped rejected", fmt.Errorf("route: %w", rejected), "rejected"},
		{"canceled", context.Canceled, "canceled"},
		{"wrapped canceled", fmt.Errorf("dial: %w", context.Canceled), "canceled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"net timeout", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, "timeout"},
		{"refused then timeout, joined", errors.Join(dialErr(refusedErrnos[0]), &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}), "timeout"},
		{"refused then timeout, sing multi", E.Errors(dialErr(refusedErrnos[0]), &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}), "timeout"},
		{"dns", &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}, "dns"},
		{"wrapped dns", fmt.Errorf("lookup: %w", &net.DNSError{Err: "server misbehaving", Name: "example.com"}), "dns"},
		{"rcode", fmt.Errorf("exchange: %w", sbdns.RcodeNameError), "dns"},
		{"refused", dialErr(refusedErrnos[0]), "refused"},
		{"net unreachable", dialErr(unreachableErrnos[0]), "unreachable"},
		{"host unreachable", dialErr(unreachableErrnos[1]), "unreachable"},
		{"reset", dialErr(resetErrnos[0]), "reset"},
		{"pipe", fmt.Errorf("write: %w", os.NewSyscallError("write", resetErrnos[1])), "reset"},
		{"eperm", dialErr(notPermittedErrnos[0]), "not-permitted"},
		{"eacces", dialErr(notPermittedErrnos[1]), "not-permitted"},
		{"eof", io.EOF, "closed"},
		{"wrapped eof", fmt.Errorf("read: %w", io.EOF), "closed"},
		{"net closed", &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}, "closed"},
		{"unknown", errors.New("something odd"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, failureClass(tc.err))
		})
	}
}

// recordingLogger keeps every DebugContext record.
type recordingLogger struct {
	sblog.ContextLogger
	mu      sync.Mutex
	records []string
}

func (l *recordingLogger) DebugContext(ctx context.Context, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, fmt.Sprint(args...))
}

// failRouter fails every routed connection with err.
type failRouter struct {
	dialRouter
	err error
}

func (r *failRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	conn.Close()
	onClose(r.err)
}

func TestBridgeLogsFailureClassNotDestination(t *testing.T) {
	const destination = "secret.example.com:443"
	logger := &recordingLogger{ContextLogger: sblog.NewNOPFactory().Logger()}
	b := &Bridge{
		Adapter: inbound.NewAdapter(constant.TypeSamizdatBridge, "bridge"),
		logger:  logger,
		router: &failRouter{err: fmt.Errorf("dial %s: %w", destination, &net.OpError{
			Op:   "dial",
			Net:  "tcp",
			Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
			Err:  os.NewSyscallError("connect", refusedErrnos[0]),
		})},
	}
	c1, c2 := net.Pipe()
	defer c2.Close()
	b.handleConnection(context.Background(), c1, destination)

	logger.mu.Lock()
	defer logger.mu.Unlock()
	require.Len(t, logger.records, 1)
	rec := logger.records[0]
	assert.True(t, strings.HasPrefix(rec, constant.TypeSamizdatBridge), rec)
	assert.Equal(t, constant.TypeSamizdatBridge+": a routed connection failed: refused", rec)
	assert.NotContains(t, rec, "secret")
	assert.NotContains(t, rec, "192.0.2.1")
}
