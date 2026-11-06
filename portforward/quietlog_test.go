package portforward

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer safe for slog's concurrent writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// exerciseLogs drives the Forwarder through every path that logs a port or
// address: a mapping added, a refused add, a failed renewal, a failed delete
// of an untracked mapping, a failed external IP query, and the PCP/NAT-PMP
// cleanup of a mapping on the wrong port.
func exerciseLogs(t *testing.T, quiet bool) string {
	t.Helper()
	setLocalIPProbeAddr(t, "127.0.0.1:53")
	buf := captureLogs(t)

	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	f.SetQuietLogs(quiet)

	_, err := f.MapPort(context.Background(), UDP, 30001, "test")
	require.NoError(t, err)

	c.mu.Lock()
	c.addErr = errors.New("router 192.168.1.1:5000 refused external port 30002")
	c.mu.Unlock()
	_, err = f.MapPort(context.Background(), TCP, 30002, "test")
	require.Error(t, err)

	f.mu.Lock()
	m := f.mappings[UDP]
	teardowns := f.teardowns
	f.mu.Unlock()
	require.NotNil(t, m)
	f.renew(m, c, teardowns)

	c.mu.Lock()
	c.deleteErr = errors.New("delete of 30003 at [fe80::1]:5351 failed")
	c.mu.Unlock()
	f.deleteUntracked(&Mapping{ExternalPort: 30003, Protocol: UDP}, c, "test")

	c.mu.Lock()
	c.extIPErr = errors.New("http://192.168.1.1:5000/ctl: status 500")
	c.mu.Unlock()
	_, err = f.ExternalIP(context.Background())
	require.Error(t, err)

	var q atomic.Bool
	q.Store(quiet)
	s := &pmpMappings{granted: map[pcpKey]uint32{}, quiet: &q}
	wrongPort := func(protocol string, internalPort, externalPort uint16, lifetime uint32) (uint16, uint32, error) {
		if lifetime == 0 {
			return 0, 0, errors.New("pcp: 192.168.1.1:5351 would not delete port 30005")
		}
		return 30005, 3600, nil
	}
	require.Error(t, s.add("pcp", wrongPort, UDP, 30004, 30004, 3600))

	return buf.String()
}

func TestQuietLogsOmitPortsAndAddresses(t *testing.T) {
	logs := exerciseLogs(t, true)

	for _, msg := range []string{
		"portforward: mapping added",
		"portforward: gateway refused the mapping",
		"portforward: lease renewal failed",
		"portforward: could not remove an untracked mapping",
		"portforward: gateway would not report its external address",
		"portforward: could not remove a mapping on the wrong port",
	} {
		assert.Contains(t, logs, msg, "quiet mode must keep the message")
	}
	// The error kinds survive, masked.
	assert.Contains(t, logs, "refused external port <n>")
	assert.Contains(t, logs, "status <n>")

	for _, leak := range []string{
		"30001", "30002", "30003", "30004", "30005", "5000", "5351",
		"127.0.0.1", "192.168.1.1", "fe80::1",
		"external_port", "internal_port", "internal_ip",
	} {
		assert.NotContains(t, logs, leak, "quiet logs leak %q", leak)
	}
}

func TestLoudLogsByDefault(t *testing.T) {
	logs := exerciseLogs(t, false)
	assert.Contains(t, logs, "external_port=30001", "default logs keep the port")
	assert.Contains(t, logs, "internal_ip=127.0.0.1", "default logs keep the address")
	assert.Contains(t, logs, "192.168.1.1", "default logs keep error text as is")
}

func TestRedactLogArgsDiscoveryErrors(t *testing.T) {
	got := redactLogArgs(true, []any{
		"method", "upnp-igd1",
		"igd2_err", "Post http://192.168.1.1:5000/ctl: refused",
		"pmp_err", errors.New("read udp 10.0.0.2:61234->10.0.0.1:5351: timeout"),
		protocolAttr(UDP),
	})
	require.Len(t, got, 7)
	assert.Equal(t, "upnp-igd1", got[1], "non-error strings are kept")
	assert.Equal(t, "Post <addr> refused", got[3])
	assert.Equal(t, "read udp <addr> timeout", got[5])
	assert.Equal(t, protocolAttr(UDP), got[6])
	assert.Equal(t, []any{"internal_port", 1}, redactLogArgs(false, []any{"internal_port", 1}), "loud mode passes args through")
}

func TestRedactTextEndpoints(t *testing.T) {
	for in, want := range map[string]string{
		`Post "http://[fdab::]:5000/ctl": connection refused`:                          "Post <addr> connection refused",
		`Post "http://router.lan:5000/ctl": dial tcp: lookup router.lan: no such host`: "Post <addr> dial tcp: lookup <addr> no such host",
		"dial udp fdab:: : network is unreachable":                                     "dial udp <addr> : network is unreachable",
		"read udp [fe80::1%en0]:5351: i/o timeout":                                     "read udp <addr> i/o timeout",
		"dial tcp ::1: connection refused":                                             "dial tcp <addr> connection refused",
		"pcp: gateway assigned external port 30005, not 30004":                         "pcp: gateway assigned external port <n>, not <n>",
		"http://192.168.1.1:5000/ctl: status 500":                                      "<addr> status <n>",
	} {
		got := redactText(in)
		assert.Equal(t, want, got, "redacting %q", in)
		for _, leak := range []string{"fdab", "router.lan", "fe80", "192.168", "5000", "5351", "3000"} {
			assert.NotContains(t, got, leak)
		}
	}
}
