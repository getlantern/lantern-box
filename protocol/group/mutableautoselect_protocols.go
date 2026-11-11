package group

import (
	"time"

	C "github.com/sagernet/sing-box/constant"

	lConst "github.com/getlantern/lantern-box/constant"
)

// protocolBehavior captures the per-protocol knobs needed when probing a
// candidate.
type protocolBehavior struct {
	probeTimeout time.Duration
	// excludeFromPool: protocols that run alongside the group with their
	// own connection manager (tor) and never belong in the candidate pool.
	excludeFromPool bool
	// lastResort: protocols that are only worth carrying traffic when no
	// other member is healthy (unbounded: a WebRTC hop through a volunteer
	// peer, slow to establish and capacity-limited). They are probed
	// asynchronously with probeTimeout, so a slow handshake never holds up
	// a probe wave, and rank in their own tier below every clean or
	// soft-demoted member.
	lastResort bool
}

// behaviorFor returns a protocol's probing knobs. A probe's timeout covers
// the dial, the handshake and the whole bandit probe body, so every protocol
// gets at least defaultProbeTimeout: a healthy route through a triangle
// frontend from Iran often needs 1.6-2s for that, and a client that gives up
// mid-body makes the proxy report the route as stalled.
func behaviorFor(outboundType string) protocolBehavior {
	switch outboundType {
	case lConst.TypeOutline:
		return protocolBehavior{probeTimeout: 10000 * time.Millisecond}
	case C.TypeTor:
		return protocolBehavior{excludeFromPool: true}
	case lConst.TypeUnbounded:
		// Signaling, ICE/NAT traversal and the egress handshake routinely
		// take tens of seconds.
		return protocolBehavior{probeTimeout: 60 * time.Second, lastResort: true}
	default:
		return protocolBehavior{probeTimeout: defaultProbeTimeout}
	}
}

const defaultProbeTimeout = 3000 * time.Millisecond
