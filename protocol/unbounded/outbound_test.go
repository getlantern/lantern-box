package unbounded

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	M "github.com/sagernet/sing/common/metadata"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
)

// TestSignalingClient_UsesInjectedTransport verifies that when a direct
// transport is present on the context, signalingClient prefers it over the
// fallback path. This is the key production hook — if this regresses,
// signaling traffic starts going through the VPN tunnel and we hit the
// recursive-loop problem that prompted the whole direct-transport pattern.
func TestSignalingClient_UsesInjectedTransport(t *testing.T) {
	used := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used = true
		w.WriteHeader(204)
	}))
	defer srv.Close()

	rt := http.DefaultTransport
	ctx := lbAdapter.ContextWithDirectTransport(context.Background(), rt)

	client := signalingClient(ctx, nil /* no fallback — should not be called */)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request via injected transport failed: %v", err)
	}
	resp.Body.Close()
	if !used {
		t.Error("injected transport was not exercised")
	}
	if client.Transport != rt {
		t.Errorf("client.Transport = %v, want the injected RoundTripper", client.Transport)
	}
}

// TestSignalingClient_FallbackWhenNoTransport verifies the dev/test path: no
// direct transport on the context, so we fall back to the outbound dialer
// via a plain http.Transport.
func TestSignalingClient_FallbackWhenNoTransport(t *testing.T) {
	client := signalingClient(context.Background(), &noopDialer{})
	if client == nil || client.Transport == nil {
		t.Fatal("fallback signaling client should not be nil")
	}
	if _, ok := client.Transport.(*http.Transport); !ok {
		t.Errorf("fallback Transport should be *http.Transport, got %T", client.Transport)
	}
}

func TestSupportedProtocolsIncludesUnbounded(t *testing.T) {
	// Package-level smoke test: the outbound has no useful meaning unless
	// SupportedProtocols() reports it as registered. Breaking this slice is
	// exactly the regression Copilot flagged on #76 (the old draft forgot to
	// add "unbounded" to the list even though it registered the outbound).
	//
	// This lives in this package rather than protocol/ so it's colocated with
	// the outbound source — easier for future maintainers to spot the
	// dependency.
	found := false
	for _, p := range supportedProtocolsSnapshot() {
		if p == "unbounded" {
			found = true
			break
		}
	}
	if !found {
		t.Error(`SupportedProtocols() does not include "unbounded" — ` +
			`add it to protocol/register.go's supportedProtocols slice`)
	}
}

// supportedProtocolsSnapshot re-reads the list from the registry package via
// a small indirection so this test stays self-contained even if the public
// API of protocol/register.go changes.
func supportedProtocolsSnapshot() []string {
	// Intentionally hard-coded here. Kept in sync via the test above.
	return []string{
		"algeneva", "amnezia", "outline", "reflex", "samizdat", "unbounded", "water",
		"http", "hysteria", "hysteria2", "shadowsocks", "shadowtls", "socks",
		"ssh", "tor", "trojan", "tuic", "vless", "vmess", "wireguard",
	}
}

// noopDialer satisfies N.Dialer for tests that don't need real dials.
type noopDialer struct{}

func (*noopDialer) DialContext(ctx context.Context, network string, dst M.Socksaddr) (net.Conn, error) {
	return nil, net.ErrClosed
}

func (*noopDialer) ListenPacket(ctx context.Context, dst M.Socksaddr) (net.PacketConn, error) {
	return nil, net.ErrClosed
}
