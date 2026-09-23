//go:build !with_quic

package protocol_test

import (
	"slices"
	"testing"

	"github.com/getlantern/lantern-box/protocol"
)

// TestSupportedProtocolsExcludesHysteria2XWithoutQUIC guards the other half: a
// build without QUIC can't run hysteria2x, so advertising it would get the
// client assigned tracks it can never dial.
func TestSupportedProtocolsExcludesHysteria2XWithoutQUIC(t *testing.T) {
	if slices.Contains(protocol.SupportedProtocols(), "hysteria2x") {
		t.Error(`non-QUIC build: protocol.SupportedProtocols() advertises "hysteria2x"`)
	}
}
