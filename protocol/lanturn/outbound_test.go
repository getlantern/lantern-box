package lanturn

import (
	"context"
	"strings"
	"testing"

	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"

	lbopt "github.com/getlantern/lantern-box/option"
)

// TestNewOutbound_RequiresCoturnEndpoints asserts NewOutbound rejects an
// empty fleet. The outbound has no production-meaningful default for this —
// without a coturn endpoint there's nowhere to allocate against — so we
// fail fast at config-time rather than at the first DialContext call.
func TestNewOutbound_RequiresCoturnEndpoints(t *testing.T) {
	_, err := NewOutbound(context.Background(), nil, logger.NOP(), "lanturn-test",
		lbopt.LanturnOutboundOptions{
			PeerAddr:          "127.0.0.1:9999",
			LanturnAuthSecret: "secret",
		})
	if err == nil {
		t.Fatal("expected error for missing CoturnEndpoints; got nil")
	}
	if !strings.Contains(err.Error(), "coturn endpoint") {
		t.Errorf("error should mention coturn endpoint requirement, got: %v", err)
	}
}

// TestNewOutbound_RequiresPeerAddr asserts NewOutbound rejects an empty
// PeerAddr — the egress address is currently a required scalar and has no
// safe default (an empty string would silently produce a TURN allocation
// pointed at nothing).
func TestNewOutbound_RequiresPeerAddr(t *testing.T) {
	_, err := NewOutbound(context.Background(), nil, logger.NOP(), "lanturn-test",
		lbopt.LanturnOutboundOptions{
			CoturnEndpoints:   []lbopt.LanturnCoturnEndpoint{{UDPAddr: "host:3478"}},
			LanturnAuthSecret: "secret",
		})
	if err == nil {
		t.Fatal("expected error for missing PeerAddr; got nil")
	}
	if !strings.Contains(err.Error(), "peer_addr") {
		t.Errorf("error should mention peer_addr requirement, got: %v", err)
	}
}

// TestNewOutbound_RequiresAuthSecret asserts NewOutbound rejects an empty
// LanturnAuthSecret. The v0.1 outbound generates OAUTH credentials from
// this secret; an empty secret would produce HMAC-of-nothing creds that
// coturn would reject (or worse, on a misconfigured server, accept).
func TestNewOutbound_RequiresAuthSecret(t *testing.T) {
	_, err := NewOutbound(context.Background(), nil, logger.NOP(), "lanturn-test",
		lbopt.LanturnOutboundOptions{
			CoturnEndpoints: []lbopt.LanturnCoturnEndpoint{{UDPAddr: "host:3478"}},
			PeerAddr:        "127.0.0.1:9999",
		})
	if err == nil {
		t.Fatal("expected error for missing LanturnAuthSecret; got nil")
	}
	if !strings.Contains(err.Error(), "lanturn_auth_secret") {
		t.Errorf("error should mention lanturn_auth_secret requirement, got: %v", err)
	}
}

// TestNewOutbound_ValidConfig asserts a minimal valid config produces an
// Outbound with no error and the right type / tag / network advertisement.
func TestNewOutbound_ValidConfig(t *testing.T) {
	ob, err := NewOutbound(context.Background(), nil, logger.NOP(), "lanturn-test",
		lbopt.LanturnOutboundOptions{
			CoturnEndpoints:   []lbopt.LanturnCoturnEndpoint{{UDPAddr: "host:3478"}},
			PeerAddr:          "127.0.0.1:9999",
			LanturnAuthSecret: "secret",
		})
	if err != nil {
		t.Fatalf("expected nil error for valid config; got: %v", err)
	}
	if ob == nil {
		t.Fatal("expected non-nil Outbound")
	}
	if ob.Tag() != "lanturn-test" {
		t.Errorf("Tag() = %q, want %q", ob.Tag(), "lanturn-test")
	}
	// TCP-only advertisement — the outbound's UDP support is gated until
	// pkg/lanturn implements UDP destinations. Asserts the regression from
	// the earlier draft (which advertised both TCP and UDP and then failed
	// at runtime on UDP dials) doesn't return.
	networks := ob.Network()
	if len(networks) != 1 || networks[0] != "tcp" {
		t.Errorf("Network() = %v, want exactly [\"tcp\"]", networks)
	}
}

// TestDialContext_AlphaError asserts DialContext fails fast with a clear
// message rather than returning a misrouted connection. This is the
// behavioral contract called out in the package doc — "the outbound is
// not yet operational" — and it's load-bearing: a hidden silent success
// here would forward proxy bytes to a hardcoded egress destination instead
// of the user's actual destination.
func TestDialContext_AlphaError(t *testing.T) {
	ob, err := NewOutbound(context.Background(), nil, logger.NOP(), "lanturn-test",
		lbopt.LanturnOutboundOptions{
			CoturnEndpoints:   []lbopt.LanturnCoturnEndpoint{{UDPAddr: "host:3478"}},
			PeerAddr:          "127.0.0.1:9999",
			LanturnAuthSecret: "secret",
		})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	dest := M.ParseSocksaddrHostPort("example.com", 443)
	_, err = ob.(*Outbound).DialContext(context.Background(), "tcp", dest)
	if err == nil {
		t.Fatal("expected error from alpha DialContext; got nil — would silently misroute bytes")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error should explain v0.1 limitation, got: %v", err)
	}
}

// TestDialContext_UDPRejected asserts DialContext also rejects UDP cleanly
// (defense-in-depth — the Adapter's advertised networks already exclude
// UDP, so routing shouldn't pick this outbound for UDP traffic, but if it
// somehow does we want a clear error rather than a "not implemented" that
// suggests the TCP path might work).
func TestDialContext_UDPRejected(t *testing.T) {
	ob, err := NewOutbound(context.Background(), nil, logger.NOP(), "lanturn-test",
		lbopt.LanturnOutboundOptions{
			CoturnEndpoints:   []lbopt.LanturnCoturnEndpoint{{UDPAddr: "host:3478"}},
			PeerAddr:          "127.0.0.1:9999",
			LanturnAuthSecret: "secret",
		})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	dest := M.ParseSocksaddrHostPort("example.com", 443)
	_, err = ob.(*Outbound).DialContext(context.Background(), "udp", dest)
	if err == nil {
		t.Fatal("expected error for UDP DialContext; got nil")
	}
	if !strings.Contains(err.Error(), "TCP only") {
		t.Errorf("UDP error should mention TCP-only, got: %v", err)
	}
}
