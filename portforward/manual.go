package portforward

import (
	"context"
	"fmt"
	"strconv"
)

// ManualForwarder exposes the same Map/Unmap/StartRenewal/ExternalIP
// surface as Forwarder but does no UPnP work. The user is expected to
// have configured a port forward on their router by hand and supplied
// the port number out-of-band. This implementation reports the same
// value for both the external and internal port — callers needing
// distinct external/internal ports should use the UPnP-based Forwarder
// (which can negotiate them) or build their own portForwarder.
//
// Use case: networks where UPnP is disabled or unavailable (router has
// UPnP off for security, ISP-provided gateways without IGD, networks
// behind double-NAT). The UPnP-based Forwarder fails in those
// environments; this type lets callers bypass discovery entirely.
type ManualForwarder struct {
	port uint16
}

// NewManualForwarder builds a ManualForwarder for a pre-configured
// router port forward. port must be in 1..65535; the caller is
// responsible for validating its input before calling.
func NewManualForwarder(port uint16) *ManualForwarder {
	return &ManualForwarder{port: port}
}

// Port returns the router port this forwarder was configured with.
func (m *ManualForwarder) Port() uint16 { return m.port }

// ParseManualPort parses a string into a TCP port number. Values outside
// 1..65535 return an error so callers can log and fall through to UPnP
// discovery rather than register a non-listening port with the server.
func ParseManualPort(s string) (uint16, error) {
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range (1..65535)", p)
	}
	return uint16(p), nil
}

// MapPort reports the configured port as both external and internal. The
// router-side rule is already in place; nothing to do at the protocol
// layer. Returns an error if the forwarder was constructed with port==0
// (not a valid listen/registration port) so callers can fall through to
// UPnP rather than register a port no peer will listen on.
//
// Protocol is set to "TCP" to match the UPnP-based Forwarder, which
// hard-codes the same value. Samizdat-in inbound traffic is TCP-only;
// when UDP support is added the two forwarders should be widened
// together.
func (m *ManualForwarder) MapPort(_ context.Context, _ uint16, _ string) (*Mapping, error) {
	if m.port == 0 {
		return nil, fmt.Errorf("manual forwarder constructed with port=0")
	}
	return &Mapping{
		ExternalPort: m.port,
		InternalPort: m.port,
		Protocol:     "TCP",
		Method:       "manual",
	}, nil
}

// UnmapPort is a no-op: the user owns the router rule and is responsible
// for removing it.
func (m *ManualForwarder) UnmapPort(_ context.Context) error { return nil }

// StartRenewal is a no-op: manually-configured rules don't carry a UPnP
// lease and don't need refreshing.
func (m *ManualForwarder) StartRenewal(_ context.Context) {}

// ExternalIP returns the empty string deliberately. With a manual port
// forward we have no UPnP gateway to ask for the WAN address, and
// probing a public IP service from the client adds a network roundtrip
// for information lantern-cloud already has — the server observes the
// peer's source address on the register call and uses that as the
// canonical external IP when this field is empty.
func (m *ManualForwarder) ExternalIP(_ context.Context) (string, error) {
	return "", nil
}
