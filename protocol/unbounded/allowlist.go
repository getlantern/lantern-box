package unbounded

import (
	"context"
	"errors"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// poolAllowlist is the residential allowlist a pool direct exit admits, matched by sing-box's
// own inline rule set, as the exit box's router matches it. A nil one admits nothing.
type poolAllowlist struct {
	set *rule.LocalRuleSet
}

func newPoolAllowlist(ctx context.Context, lg logger.ContextLogger, rs *option.RuleSet) (*poolAllowlist, error) {
	if rs == nil {
		return nil, nil
	}
	if rs.Type != C.RuleSetTypeInline {
		return nil, errors.New("unbounded: direct_exit.pool_allowlist is not an inline rule set")
	}
	set, err := rule.NewLocalRuleSet(ctx, lg, "unbounded-pool-allowlist", *rs)
	if err != nil {
		return nil, err
	}
	return &poolAllowlist{set: set}, nil
}

// admits reports whether a pool exit admits network to destination, before DNS as the exit box
// does: a hostname matches its domain grants, and an IP literal only the CIDR grants.
func (a *poolAllowlist) admits(network string, destination M.Socksaddr) bool {
	if a == nil {
		return false
	}
	return a.set.Match(&adapter.InboundContext{Network: N.NetworkName(network), Destination: destination})
}
