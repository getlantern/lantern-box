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
	// substituteDelay, when non-zero, replaces the measured probe delay
	// for ranking. Set for protocols whose handshake jitter makes RTT
	// meaningless (samizdat).
	substituteDelay time.Duration
}

func behaviorFor(outboundType string) protocolBehavior {
	switch outboundType {
	case lConst.TypeALGeneva:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case lConst.TypeAmnezia:
		return protocolBehavior{probeTimeout: 1500 * time.Millisecond}
	case C.TypeHTTP:
		return protocolBehavior{probeTimeout: 2000 * time.Millisecond}
	case C.TypeHysteria:
		return protocolBehavior{probeTimeout: 1500 * time.Millisecond}
	case C.TypeHysteria2, lConst.TypeHysteria2X:
		return protocolBehavior{probeTimeout: 1500 * time.Millisecond}
	case lConst.TypeOutline:
		return protocolBehavior{probeTimeout: 10000 * time.Millisecond}
	case lConst.TypeReflex:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case lConst.TypeSamizdat:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case C.TypeShadowsocks:
		return protocolBehavior{probeTimeout: 2000 * time.Millisecond}
	case C.TypeShadowTLS:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case C.TypeSOCKS:
		return protocolBehavior{probeTimeout: 2000 * time.Millisecond}
	case C.TypeSSH:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case C.TypeTor:
		return protocolBehavior{excludeFromPool: true}
	case C.TypeTrojan:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case C.TypeTUIC:
		return protocolBehavior{probeTimeout: 1500 * time.Millisecond}
	case lConst.TypeUnbounded:
		// Signaling, ICE/NAT traversal and the egress handshake routinely
		// take tens of seconds.
		return protocolBehavior{probeTimeout: 60 * time.Second, lastResort: true}
	case C.TypeVLESS:
		return protocolBehavior{probeTimeout: 2000 * time.Millisecond}
	case C.TypeVMess:
		return protocolBehavior{probeTimeout: 2000 * time.Millisecond}
	case C.TypeWireGuard:
		return protocolBehavior{probeTimeout: 1500 * time.Millisecond}
	default:
		return protocolBehavior{probeTimeout: 2000 * time.Millisecond}
	}
}
