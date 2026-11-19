package samizdatreverse

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	sblog "github.com/sagernet/sing-box/log"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/constant"
)

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
			Err:  os.NewSyscallError("connect", syscall.ECONNREFUSED),
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
