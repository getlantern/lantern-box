package unbounded_test

import (
	"context"
	"fmt"
	"testing"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbox "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/protocol/unbounded"
)

// parityAllowlist has the shape of lantern-cloud's residential allowlist (pcfg's
// peerAllowlistPolicy): per network, one grant of domains and suffixes and one of CIDRs, each
// with its ports.
const parityAllowlist = `{"type": "inline", "tag": "allowlist", "rules": [
	{"network": ["tcp"], "domain": ["allowed.test"], "domain_suffix": ["suffix.test"], "port": [80, 443]},
	{"network": ["tcp"], "ip_cidr": ["203.0.113.0/24", "2001:db8::/32"], "port": [80, 443]},
	{"network": ["udp"], "domain": ["allowed.test"], "domain_suffix": ["suffix.test"], "port": [443]},
	{"network": ["udp"], "ip_cidr": ["203.0.113.0/24", "2001:db8::/32"], "port": [443]},
	{"network": ["tcp"], "domain_suffix": ["chat.test"], "port": [5222, 5228, 5229, 5230]}
]}`

// parityBox is a pool exit box's pre-DNS allowlist check, as lantern-cloud's applyPeerAllowlist
// writes it: a reject of whatever the allowlist doesn't match.
const parityBox = `{
	"log": {"disabled": true},
	"outbounds": [{"type": "direct", "tag": "direct"}],
	"route": {
		"rule_set": [%s],
		"rules": [
			{"rule_set": ["allowlist"], "invert": true, "action": "reject"},
			{"action": "route", "outbound": "direct"}
		]
	}
}`

// The consumer's allowlist check agrees with a real sing-box router enforcing the same rule
// set, across domains, suffixes, IP literals, networks and ports.
func TestPoolAllowlistMatchesTheExitBoxRouter(t *testing.T) {
	ctx := service.ExtendContext(lbox.BaseContext())
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(parityBox, parityAllowlist)))
	require.NoError(t, err)
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, b.Start())
	t.Cleanup(func() { _ = b.Close() })

	rs, err := json.UnmarshalExtended[option.RuleSet]([]byte(parityAllowlist))
	require.NoError(t, err)

	admitted := 0
	for _, tc := range []struct{ network, dest string }{
		{"tcp", "allowed.test:443"},
		{"tcp", "allowed.test:80"},
		{"tcp", "ALLOWED.test:443"},
		{"tcp", "allowed.test:22"},
		{"tcp", "www.allowed.test:443"},
		{"tcp", "suffix.test:443"},
		{"tcp", "a.b.suffix.test:80"},
		{"tcp", "notsuffix.test:443"},
		{"tcp", "other.test:443"},
		{"udp", "allowed.test:443"},
		{"udp", "allowed.test:80"},
		{"udp", "x.suffix.test:443"},
		{"tcp", "203.0.113.7:443"},
		{"tcp", "203.0.113.7:8080"},
		{"tcp", "198.51.100.7:443"},
		{"udp", "203.0.113.7:443"},
		{"udp", "203.0.113.7:53"},
		{"tcp", "[2001:db8::1]:443"},
		{"tcp", "[2001:db9::1]:443"},
		{"tcp", "im.chat.test:5222"},
		{"tcp", "im.chat.test:443"},
		{"udp", "im.chat.test:5222"},
	} {
		dest := M.ParseSocksaddr(tc.dest)
		rejected := b.Router().PreMatch(adapter.InboundContext{Network: tc.network, Destination: dest}, nil).Action == adapter.PreMatchReject
		admits, err := unbounded.PoolAllowlistAdmits(context.Background(), &rs, tc.network, dest)
		require.NoError(t, err)
		assert.Equal(t, !rejected, admits, "%s %s", tc.network, tc.dest)
		if admits {
			admitted++
		}
	}
	assert.Equal(t, 11, admitted, "the table exercises both outcomes")
}
