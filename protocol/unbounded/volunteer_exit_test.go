package unbounded_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbox "github.com/getlantern/lantern-box"
	LO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/peerrules"
	"github.com/getlantern/lantern-box/protocol/unbounded"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// directExitBoxConfig is a volunteer's box config with the rules peerrules checks a direct
// exit for, its abuse rule sets inline so the box downloads nothing.
const directExitBoxConfig = `{
	"log": {"disabled": true},
	"outbounds": [{"type": "direct", "tag": "direct"}],
	"route": {
		"rule_set": [
			{"type": "inline", "tag": "geosite-malware", "rules": [{"domain": ["malware.test"]}]},
			{"type": "inline", "tag": "geoip-malware", "rules": [{"ip_cidr": ["192.0.2.66/32"]}]},
			{"type": "inline", "tag": "geosite-phishing", "rules": [{"domain": ["phishing.test"]}]},
			{"type": "inline", "tag": "geosite-cryptominers", "rules": [{"domain": ["cryptominer.test"]}]}
		],
		"rules": [
			{"action": "resolve"},
			{"action": "reject", "ip_is_private": true},
			{"action": "reject", "rule_set": ["geosite-malware", "geoip-malware", "geosite-phishing", "geosite-cryptominers"]},
			{"action": "reject", "ip_cidr": ["10.0.0.0/8", "127.0.0.0/8"]},
			{"action": "reject", "port": [25, 465, 587]},
			{"action": "route", "outbound": "direct", "udp_connect": true}
		]
	},
	"services": [{
		"type": "unbounded",
		"tag": "volunteer",
		"discovery_srv": %q,
		"egress_addr": %q,
		"stun_servers": ["stun:127.0.0.1:9"],
		"stats_interval": -1,
		"direct_exit": %v
	}]
}`

// With direct_exit, the volunteer serves exits through its own box's router, so broflake
// offers them; the box's route applies to them as to every other inbound. It does so only in a
// box built with the context peerrules.CheckDirectExit returns for the box's options: any
// other box gets a relay only. Without with_quic there is no exit and the volunteer serves as
// before.
func TestVolunteerDirectExitHooksIntoBroflake(t *testing.T) {
	probe, err := exit.New(context.Background(), sblog.NewNOPFactory().Logger())
	available := !errors.Is(err, exit.ErrUnavailable)
	if probe != nil {
		probe.Close()
	}
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)

	for name, tc := range map[string]struct {
		directExit, checked, exits bool
	}{
		"checked direct exit":   {directExit: true, checked: true, exits: true},
		"unchecked direct exit": {directExit: true},
		"no direct exit":        {checked: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := service.ExtendContext(lbox.BaseContext())
			opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(directExitBoxConfig,
				unreachable.URL, "ws"+unreachable.URL[len("http"):], tc.directExit)))
			require.NoError(t, err)
			if tc.checked {
				var stripped bool
				ctx, stripped = peerrules.CheckDirectExit(ctx, &opts)
				require.False(t, stripped, "the config passes the direct exit check")
			}
			got, ok := opts.Services[0].Options.(*LO.UnboundedVolunteerOptions)
			require.True(t, ok)
			require.Equal(t, tc.directExit, got.DirectExit)

			b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
			require.NoError(t, err)
			closed := false
			closeBox := func() {
				if closed {
					return
				}
				closed = true
				done := make(chan error, 1)
				go func() { done <- b.Close() }()
				select {
				case err := <-done:
					assert.NoError(t, err)
				case <-time.After(20 * time.Second):
					t.Error("closing the box hung")
				}
			}
			t.Cleanup(closeBox)
			require.NoError(t, b.Start())

			svc, ok := service.FromContext[adapter.ServiceManager](ctx).Get("volunteer")
			require.True(t, ok)
			e := unbounded.VolunteerExit(svc)
			if !tc.exits || !available {
				assert.Nil(t, e, "no exit: the volunteer serves as a relay")
				return
			}
			require.NotNil(t, e)
			assert.True(t, e.Ready(), "the exit is on, with the box's router")
			closeBox()
			assert.False(t, e.Ready(), "closing the volunteer closes its exit")
		})
	}
}
