package option

import "github.com/sagernet/sing-box/option"

// TestingOutboundOptions configures a testing outbound, a direct egress dialer.
// The destination is supplied at dial time, so the only options are the dialer's.
type TestingOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions
}
