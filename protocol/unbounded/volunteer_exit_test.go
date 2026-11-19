package unbounded_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"github.com/getlantern/lantern-box/internal/directexit"
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
			assert.True(t, e.Ready(false), "the exit is on, with the box's router")
			assert.False(t, e.Ready(true), "a node's box carries the pool policy alone")
			closeBox()
			assert.False(t, e.Ready(false), "closing the volunteer closes its exit")
		})
	}
}

// A box whose context carries the deadline of the registration behind it, as a node's does,
// exits only until then. The exit itself ends its sessions at the deadline.
func TestVolunteerDirectExitEndsAtItsDeadline(t *testing.T) {
	probe, err := exit.New(context.Background(), sblog.NewNOPFactory().Logger())
	if errors.Is(err, exit.ErrUnavailable) {
		t.Skip("built without with_quic")
	}
	probe.Close()
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)

	for name, tc := range map[string]struct {
		deadline time.Duration
		ready    bool
	}{
		"passed":   {deadline: -time.Second},
		"upcoming": {deadline: 2 * time.Second, ready: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := service.ExtendContext(lbox.BaseContext())
			opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(directExitBoxConfig,
				unreachable.URL, "ws"+unreachable.URL[len("http"):], true)))
			require.NoError(t, err)
			ctx, _ = peerrules.CheckDirectExit(ctx, &opts)
			ctx = directexit.WithValidUntil(ctx, time.Now().Add(tc.deadline))
			b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
			require.NoError(t, err)
			t.Cleanup(func() { _ = b.Close() })
			require.NoError(t, b.Start())

			svc, ok := service.FromContext[adapter.ServiceManager](ctx).Get("volunteer")
			require.True(t, ok)
			e := unbounded.VolunteerExit(svc)
			require.NotNil(t, e)
			assert.Equal(t, tc.ready, e.Ready(false))
			assert.Eventually(t, func() bool { return !e.Ready(false) }, 10*time.Second, 20*time.Millisecond,
				"the exit stops at the deadline")
		})
	}
}

// sing-box starts a box whose remote rule set failed to download with the set empty, so its
// abuse rejects would match nothing. The volunteer offers no direct exit until every abuse rule
// set has loaded, and offers them once a later download succeeds.
func TestVolunteerDirectExitWaitsForItsAbuseRuleSets(t *testing.T) {
	probe, err := exit.New(context.Background(), sblog.NewNOPFactory().Logger())
	if errors.Is(err, exit.ErrUnavailable) {
		t.Skip("built without with_quic")
	}
	probe.Close()
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)

	for name, failures := range map[string]int32{
		"never loads":      1 << 30,
		"loads on a retry": 1,
	} {
		t.Run(name, func(t *testing.T) {
			var served atomic.Int32
			lists := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if served.Add(1) <= failures {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(`{"version": 3, "rules": [{"domain": ["malware.test"]}]}`))
			}))
			t.Cleanup(lists.Close)
			config := strings.Replace(directExitBoxConfig,
				`{"type": "inline", "tag": "geosite-malware", "rules": [{"domain": ["malware.test"]}]}`,
				fmt.Sprintf(`{"type": "remote", "tag": "geosite-malware", "format": "source", "url": %q, "download_detour": "direct"}`, lists.URL), 1)
			ctx := service.ExtendContext(lbox.BaseContext())
			opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(config,
				unreachable.URL, "ws"+unreachable.URL[len("http"):], true)))
			require.NoError(t, err)
			ctx, stripped := peerrules.CheckDirectExit(ctx, &opts)
			require.False(t, stripped)
			b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
			require.NoError(t, err)
			t.Cleanup(func() { _ = b.Close() })
			require.NoError(t, b.Start())

			svc, ok := service.FromContext[adapter.ServiceManager](ctx).Get("volunteer")
			require.True(t, ok)
			e := unbounded.VolunteerExit(svc)
			require.NotNil(t, e)
			if failures > 1 {
				assert.Never(t, func() bool { return e.Ready(false) }, time.Second, 20*time.Millisecond,
					"a direct exit while its abuse rule sets are empty")
				return
			}
			assert.Eventually(t, func() bool { return e.Ready(false) }, 10*time.Second, 20*time.Millisecond,
				"the direct exit once its abuse rule sets loaded")
		})
	}
}

// A rule set's later reload can leave it empty, as a local file emptied, or a download of an
// empty list, does. The volunteer withdraws its direct exit while any abuse rule set is empty,
// and offers it again once the set holds rules.
func TestVolunteerDirectExitFollowsItsAbuseRuleSets(t *testing.T) {
	probe, err := exit.New(context.Background(), sblog.NewNOPFactory().Logger())
	if errors.Is(err, exit.ErrUnavailable) {
		t.Skip("built without with_quic")
	}
	probe.Close()
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)
	const full, empty = `{"version": 3, "rules": [{"domain": ["malware.test"]}]}`, `{"version": 3, "rules": []}`
	path := filepath.Join(t.TempDir(), "malware.json")
	require.NoError(t, os.WriteFile(path, []byte(full), 0o600))
	config := strings.Replace(directExitBoxConfig,
		`{"type": "inline", "tag": "geosite-malware", "rules": [{"domain": ["malware.test"]}]}`,
		fmt.Sprintf(`{"type": "local", "tag": "geosite-malware", "format": "source", "path": %q}`, path), 1)
	ctx := service.ExtendContext(lbox.BaseContext())
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(config,
		unreachable.URL, "ws"+unreachable.URL[len("http"):], true)))
	require.NoError(t, err)
	ctx, stripped := peerrules.CheckDirectExit(ctx, &opts)
	require.False(t, stripped)
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start())
	svc, ok := service.FromContext[adapter.ServiceManager](ctx).Get("volunteer")
	require.True(t, ok)
	e := unbounded.VolunteerExit(svc)
	require.NotNil(t, e)
	require.True(t, e.Ready(false))

	require.NoError(t, os.WriteFile(path, []byte(empty), 0o600))
	assert.Eventually(t, func() bool { return !e.Ready(false) }, 10*time.Second, 20*time.Millisecond,
		"the direct exit is withdrawn once a set is empty")
	require.NoError(t, os.WriteFile(path, []byte(full), 0o600))
	assert.Eventually(t, func() bool { return e.Ready(false) }, 10*time.Second, 20*time.Millisecond,
		"and offered again once it holds rules")
}

// sing-box starts services before its router's PostStart, which is when the route rules take up
// their rule sets: until then the abuse rejects match nothing, whatever the sets hold. The
// volunteer offers its direct exit only from its own PostStart, which follows the router's.
func TestVolunteerDirectExitWaitsForPostStart(t *testing.T) {
	probe, err := exit.New(context.Background(), sblog.NewNOPFactory().Logger())
	if errors.Is(err, exit.ErrUnavailable) {
		t.Skip("built without with_quic")
	}
	probe.Close()
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)
	ctx := service.ExtendContext(lbox.BaseContext())
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(directExitBoxConfig,
		unreachable.URL, "ws"+unreachable.URL[len("http"):], true)))
	require.NoError(t, err)
	ctx, stripped := peerrules.CheckDirectExit(ctx, &opts)
	require.False(t, stripped)
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	svc, ok := service.FromContext[adapter.ServiceManager](ctx).Get("volunteer")
	require.True(t, ok)

	// The stages as sing-box runs them for a service, without the router's.
	require.NoError(t, svc.Start(adapter.StartStateStart))
	e := unbounded.VolunteerExit(svc)
	require.NotNil(t, e)
	assert.False(t, e.Ready(false), "no direct exit before PostStart")
	require.NoError(t, svc.Start(adapter.StartStatePostStart))
	assert.True(t, e.Ready(false), "the direct exit from PostStart")
	require.NoError(t, svc.Close())
}
