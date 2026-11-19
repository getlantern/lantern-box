package unbounded

import (
	"context"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
)

// VolunteerExit is the direct exit a volunteer service offers broflake, for the tests outside
// the package, which can build boxes through peerrules.
func VolunteerExit(svc adapter.Service) *UBClientcore.ExitOptions {
	return svc.(*Volunteer).rtcOpt.Exit
}

// PoolAllowlistAdmits is the check exitPaths makes of a pool path's allowlist, for the parity
// test outside the package, which builds a box.
func PoolAllowlistAdmits(ctx context.Context, rs *option.RuleSet, network string, destination M.Socksaddr) (bool, error) {
	a, err := newPoolAllowlist(ctx, sblog.NewNOPFactory().Logger(), rs)
	if err != nil {
		return false, err
	}
	return a.admits(network, destination), nil
}
