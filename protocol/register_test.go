package protocol_test

import (
	"testing"

	"github.com/getlantern/lantern-box/protocol"
)

// TestSupportedProtocolsIncludesUnbounded asserts that the production
// SupportedProtocols() slice advertises "unbounded". This is a smoke test
// against an easy regression: it's possible to register an outbound with the
// sing-box registry but forget to add it to the slice, in which case callers
// that gate on SupportedProtocols() — the client config negotiator is the
// important one — won't know the outbound is available.
//
// The old draft at PR #76 hit exactly this bug; it registered "unbounded"
// via RegisterOutbound but omitted it from supportedProtocols.
//
// Lives in package protocol_test (external) so it can read the real slice
// via protocol.SupportedProtocols() without a circular import from
// protocol/unbounded.
func TestSupportedProtocolsIncludesUnbounded(t *testing.T) {
	for _, p := range protocol.SupportedProtocols() {
		if p == "unbounded" {
			return
		}
	}
	t.Error(`protocol.SupportedProtocols() does not include "unbounded" — ` +
		`add it to the supportedProtocols slice in protocol/register.go`)
}

// TestSupportedProtocolsIncludesLanturn mirrors the Unbounded test for the
// new lanturn outbound — same regression hazard (register without slice
// addition silently disables the outbound from the client config
// negotiator's perspective).
func TestSupportedProtocolsIncludesLanturn(t *testing.T) {
	for _, p := range protocol.SupportedProtocols() {
		if p == "lanturn" {
			return
		}
	}
	t.Error(`protocol.SupportedProtocols() does not include "lanturn" — ` +
		`add it to the supportedProtocols slice in protocol/register.go`)
}
