//go:build with_quic

package protocol_test

import (
	"slices"
	"testing"

	"github.com/getlantern/lantern-box/protocol"
)

// TestSupportedProtocolsIncludesHysteria2XWithQUIC guards the QUIC half of the
// hysteria2x contract: a with_quic build registers the outbound, so it must
// advertise it, or lantern-cloud never assigns a hysteria2x track to it.
func TestSupportedProtocolsIncludesHysteria2XWithQUIC(t *testing.T) {
	if !slices.Contains(protocol.SupportedProtocols(), "hysteria2x") {
		t.Error(`with_quic build: protocol.SupportedProtocols() does not include "hysteria2x"`)
	}
}
