package protocol_test

import (
	"slices"
	"testing"

	"github.com/getlantern/lantern-box/constant"
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

// TestSupportedProtocolsIncludesTwiddle guards the same regression for twiddle:
// registering with the sing-box registry but omitting the protocol from the
// advertised slice leaves the client negotiator unaware it exists.
func TestSupportedProtocolsIncludesTwiddle(t *testing.T) {
	for _, p := range protocol.SupportedProtocols() {
		if p == "twiddle" {
			return
		}
	}
	t.Error(`protocol.SupportedProtocols() does not include "twiddle" — ` +
		`add it to the supportedProtocols slice in protocol/register.go`)
}

// TestSupportedProtocolsIncludesSamizdatReverse guards the same regression for
// the reverse direct mode types: the portal and bridge inbounds and the
// reverse outbound.
func TestSupportedProtocolsIncludesSamizdatReverse(t *testing.T) {
	for _, want := range []string{"samizdat-portal", "samizdat-reverse", "samizdat-bridge"} {
		if !slices.Contains(protocol.SupportedProtocols(), want) {
			t.Errorf("protocol.SupportedProtocols() does not include %q — "+
				"register it in protocol/register.go", want)
		}
	}
}

// TestSupportedProtocolsIncludesDNSPool checks the pool DNS server type is
// advertised: lantern-cloud sends a pool only to clients that list it, since
// a client rejects a config naming a DNS server type it can't build.
func TestSupportedProtocolsIncludesDNSPool(t *testing.T) {
	if !slices.Contains(protocol.SupportedProtocols(), constant.TypeDNSPool) {
		t.Errorf("protocol.SupportedProtocols() does not include %q", constant.TypeDNSPool)
	}
}
