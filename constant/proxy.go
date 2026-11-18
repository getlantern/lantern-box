package constant

const (
	TypeAmnezia    = "amnezia"
	TypeALGeneva   = "algeneva"
	TypeHysteria2X = "hysteria2x"
	TypeMeek       = "meek"
	TypeOutline    = "outline"
	TypeReflex     = "reflex"
	TypeSamizdat   = "samizdat"
	TypeTwiddle    = "twiddle"
	TypeUnbounded  = "unbounded"
	TypeWATER      = "water"
)

// Reverse direct mode: a helper's portal inbound accepts the inviter's
// connections and its reverse outbound sends routed traffic back down them,
// while the inviter's bridge inbound dials out to the portal.
const (
	TypeSamizdatPortal  = "samizdat-portal"
	TypeSamizdatReverse = "samizdat-reverse"
	TypeSamizdatBridge  = "samizdat-bridge"
)

const (
	TypeBanditProbe       = "banditprobe"
	TypeFallback          = "fallback"
	TypeHedge             = "hedge"
	TypeMutableSelector   = "mutableselector"
	TypeMutableURLTest    = "mutableurltest"
	TypeMutableAutoSelect = "mutableautoselect"
)

const (
	// TypeOutboundEval is the service type a stored configuration names, so
	// changing the spelling orphans every configuration using the old one.
	TypeOutboundEval = "outbound-eval"
)
