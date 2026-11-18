package unbounded

import (
	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/adapter"
)

// VolunteerExit is the direct exit a volunteer service offers broflake, for the tests outside
// the package, which can build boxes through peerrules.
func VolunteerExit(svc adapter.Service) *UBClientcore.ExitOptions {
	return svc.(*Volunteer).rtcOpt.Exit
}
