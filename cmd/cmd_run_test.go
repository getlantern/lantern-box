package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbox "github.com/getlantern/lantern-box"
	LO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/peerrules"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

type taggedOutbound struct{ adapter.Outbound }

func (taggedOutbound) Tag() string { return "direct" }

// routeWithin routes server through m's RoutedConnection, failing if it blocks.
func routeWithin(t *testing.T, m *clientcontext.Manager, server net.Conn, inbound string) net.Conn {
	t.Helper()
	done := make(chan net.Conn, 1)
	go func() {
		done <- m.RoutedConnection(context.Background(), server, adapter.InboundContext{Inbound: inbound}, nil, taggedOutbound{})
	}()
	select {
	case c := <-done:
		return c
	case <-time.After(time.Second):
		t.Fatal("the tracker read the connection")
		return nil
	}
}

// A direct exit stream carries the user's own traffic, with no client-info frame, so the
// tracker must not hold it reading for one. Every other inbound is still read.
func TestClientContextSkipsUnboundedExit(t *testing.T) {
	m := clientcontext.NewManager(clientContextBounds(), log.NewNOPFactory().Logger())

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	assert.Same(t, server, routeWithin(t, m, server, exit.InboundTag))

	server, client = net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() { _, _ = io.WriteString(client, "hi"); _ = client.Close() }()
	routed := routeWithin(t, m, server, "samizdat-in")
	assert.NotSame(t, server, routed, "other inbounds are read for client info")
	rest, err := io.ReadAll(routed)
	require.NoError(t, err)
	assert.Equal(t, "hi", string(rest))
}

// runVolunteer is a `lantern-box run` config's volunteer that asks to exit directly.
const runVolunteer = `"services":[{"type":"unbounded","tag":"unbounded-volunteer","discovery_srv":"https://freddie.test",` +
	`"egress_addr":"wss://egress.test","stun_servers":["stun:192.0.2.1:3478"],"direct_exit":true}]`

// runAbuseRoute is a route with the abuse rules, with rules ahead of them, and a final route
// with udp_connect after.
func runAbuseRoute(rules string) string {
	return `"route":{
		"rule_set":[
			{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
		],
		"rules":[` + rules + `
			{"action":"reject","rule_set":["geosite-malware"]},
			{"action":"reject","rule_set":["geoip-malware"]},
			{"action":"reject","rule_set":["geosite-phishing"]},
			{"action":"reject","rule_set":["geosite-cryptominers"]},
			{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16"]},
			{"action":"reject","port":[25,465,587]},
			{"action":"route","outbound":"direct","udp_connect":true}
		]
	}`
}

// A config run by hand gets the same check as lantern-cloud's: its volunteer exits strangers'
// traffic directly only under rules that check it, and otherwise only relays.
func TestReadConfigDropsUncheckedDirectExit(t *testing.T) {
	prev := globalCtx
	globalCtx = lbox.BaseContext()
	t.Cleanup(func() { globalCtx = prev })
	for name, tc := range map[string]struct {
		config string
		want   bool
	}{
		"no rules":   {config: `{` + runVolunteer + `}`},
		"no resolve": {config: `{` + runVolunteer + `,` + runAbuseRoute(``) + `}`},
		"every rule": {
			config: `{` + runVolunteer + `,` + runAbuseRoute(`{"action":"resolve"},{"action":"reject","ip_is_private":true},`) + `}`,
			want:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.config), 0o600))
			ctx, opts, err := readConfig(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, peerrules.DirectExitChecked(ctx), "the box's context lets the volunteer exit")
			require.Len(t, opts.Services, 1)
			vol, ok := opts.Services[0].Options.(*LO.UnboundedVolunteerOptions)
			require.True(t, ok)
			assert.Equal(t, tc.want, vol.DirectExit)
			assert.Equal(t, "https://freddie.test", vol.DiscoverySrv, "the volunteer still relays")
		})
	}
}
