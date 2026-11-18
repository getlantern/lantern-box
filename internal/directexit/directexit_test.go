package directexit_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbox "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/internal/directexit"
)

// ruleSetConfig is a box whose geosite-malware is remote, at %q, and whose other abuse rule
// sets are inline.
const ruleSetConfig = `{
	"log": {"disabled": true},
	"outbounds": [{"type": "direct", "tag": "direct"}],
	"route": {
		"rule_set": [
			{"type": "remote", "tag": "geosite-malware", "format": "source", "url": %q, "download_detour": "direct"},
			{"type": "inline", "tag": "geoip-malware", "rules": [{"ip_cidr": ["192.0.2.66/32"]}]},
			{"type": "inline", "tag": "geosite-phishing", "rules": [{"domain": ["phishing.test"]}]},
			{"type": "inline", "tag": "geosite-cryptominers", "rules": [{"domain": ["cryptominer.test"]}]}
		],
		"rules": [
			{"action": "reject", "rule_set": ["geosite-malware", "geoip-malware", "geosite-phishing", "geosite-cryptominers"]},
			{"action": "route", "outbound": "direct"}
		]
	}
}`

// RuleSetServer serves a source rule set, or a 500 for its first failures requests.
func ruleSetServer(t *testing.T, failures int32) *httptest.Server {
	t.Helper()
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) <= failures {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"version": 3, "rules": [{"domain": ["malware.test"]}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func startBox(t *testing.T, url string) *sbox.Box {
	t.Helper()
	ctx := service.ExtendContext(lbox.BaseContext())
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(ruleSetConfig, url)))
	require.NoError(t, err)
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start())
	return b
}

func TestAbuseRuleSetsLoaded(t *testing.T) {
	b := startBox(t, ruleSetServer(t, 0).URL)
	assert.NoError(t, directexit.AbuseRuleSetsLoaded(b.Router()), "a remote set that downloaded, and inline sets")
}

// sing-box starts a box whose remote rule set failed to download, with the set empty.
func TestAbuseRuleSetsNotLoaded(t *testing.T) {
	srv := ruleSetServer(t, 1<<30)
	b := startBox(t, srv.URL)
	err := directexit.AbuseRuleSetsLoaded(b.Router())
	require.ErrorIs(t, err, directexit.ErrAbuseRuleSetsNotLoaded)
	assert.Contains(t, err.Error(), "geosite-malware")
	assert.NotContains(t, err.Error(), "geoip-malware", "only the set that failed")
	assert.NotContains(t, err.Error(), srv.URL)
}
