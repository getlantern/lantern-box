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
	// excludeFromPool: peer/network protocols (tor, unbounded) run
	// alongside the group with their own connection managers and never
	// belong in the candidate pool.
	excludeFromPool bool
	// substituteDelay, when non-zero, replaces the measured probe delay
	// for ranking. Set for protocols whose handshake jitter makes RTT
	// meaningless (samizdat).
	substituteDelay time.Duration
}

// samizdatNominalDelay is the spec-fixed synthetic delay used to rank
// samizdat candidates.
const samizdatNominalDelay = 1000 * time.Millisecond

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
	case C.TypeHysteria2:
		return protocolBehavior{probeTimeout: 1500 * time.Millisecond}
	case lConst.TypeOutline:
		return protocolBehavior{probeTimeout: 10000 * time.Millisecond}
	case lConst.TypeReflex:
		return protocolBehavior{probeTimeout: 3000 * time.Millisecond}
	case lConst.TypeSamizdat:
		return protocolBehavior{
			probeTimeout:    3000 * time.Millisecond,
			substituteDelay: samizdatNominalDelay,
		}
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
		return protocolBehavior{excludeFromPool: true}
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
