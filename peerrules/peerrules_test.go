package peerrules

import (
	"context"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	LO "github.com/getlantern/lantern-box/option"
)

// minimalValidLaunchCfg returns a launch_cfg JSON that passes
// ValidateAbuseRules: the four abuse rule_set tags (each as a
// "remote" rule_set + a matching unconditional reject rule), plus
// one RFC1918 and one SMTP canary in reject rules. Shared by the
// stub server used in Start-path tests so the existing tests do not
// regress on the new check.
const minimalValidLaunchCfg = `{
	"inbounds":[{"type":"samizdat","tag":"samizdat-in"}],
	"route":{
		"rule_set":[
			{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
		],
		"rules":[
			{"action":"reject","rule_set":["geosite-malware"]},
			{"action":"reject","rule_set":["geoip-malware"]},
			{"action":"reject","rule_set":["geosite-phishing"]},
			{"action":"reject","rule_set":["geosite-cryptominers"]},
			{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","127.0.0.0/8","169.254.0.0/16","::1/128","fc00::/7","fe80::/10"]},
			{"action":"reject","port":[25,465,587,2525,6660,6661,6662,6663,6664,6665,6666,6667,6668,6669,6697]}
		]
	}
}`

func TestValidateAbuseRules_HappyPath(t *testing.T) {
	if err := ValidateAbuseRules(minimalValidLaunchCfg); err != nil {
		t.Fatalf("expected canonical launch_cfg to pass, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRouteBlock(t *testing.T) {
	err := ValidateAbuseRules(`{"inbounds":[]}`)
	if err == nil {
		t.Fatal("expected error when route block is absent")
	}
	if !strings.Contains(err.Error(), "route block") {
		t.Errorf("error should mention route block, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRuleSetTag(t *testing.T) {
	// Drop geosite-phishing from the rule_set list. The reject rule
	// for it can stay; the check should still flag the missing tag
	// because the reject is a no-op without the rule_set.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},`,
		``, 1)
	err := ValidateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when an abuse tag is missing from route.rule_set")
	}
	if !strings.Contains(err.Error(), "geosite-phishing") {
		t.Errorf("error should name the missing tag, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRejectRule(t *testing.T) {
	// Keep all rule_sets but drop one reject rule. sing-box will
	// download the list but never enforce it.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","rule_set":["geosite-cryptominers"]},`,
		``, 1)
	err := ValidateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when an abuse tag has no reject rule")
	}
	if !strings.Contains(err.Error(), "geosite-cryptominers") {
		t.Errorf("error should name the unrejected tag, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRFC1918Canary(t *testing.T) {
	// Strip the RFC1918 reject rule. SMTP block stays — we want to
	// see the RFC1918-specific error message.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","127.0.0.0/8","169.254.0.0/16","::1/128","fc00::/7","fe80::/10"]},`,
		``, 1)
	err := ValidateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when RFC1918 reject is missing")
	}
	if !strings.Contains(err.Error(), "RFC1918") {
		t.Errorf("error should mention RFC1918, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingSMTPCanary(t *testing.T) {
	// Drop the SMTP/IRC port-reject rule. Removes the preceding
	// comma too so the resulting JSON is still well-formed (the
	// port-reject is the last entry in the rules array).
	bad := strings.Replace(minimalValidLaunchCfg,
		`,
			{"action":"reject","port":[25,465,587,2525,6660,6661,6662,6663,6664,6665,6666,6667,6668,6669,6697]}`,
		``, 1)
	err := ValidateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when SMTP port reject is missing")
	}
	if !strings.Contains(err.Error(), "SMTP") {
		t.Errorf("error should mention SMTP, got: %v", err)
	}
}

func TestValidateAbuseRules_NonRejectRulesIgnored(t *testing.T) {
	// A rule_set with action "route" (not reject) should NOT count
	// — sing-box would forward those flows to a named outbound
	// instead of dropping them. ValidateAbuseRules must demand the
	// reject action specifically.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","rule_set":["geosite-malware"]},`,
		`{"action":"route","outbound":"direct","rule_set":["geosite-malware"]},`, 1)
	err := ValidateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when abuse tag has 'route' action instead of 'reject'")
	}
	if !strings.Contains(err.Error(), "geosite-malware") {
		t.Errorf("error should name the wrongly-actioned tag, got: %v", err)
	}
}

func TestValidateAbuseRules_AllErrorsReported(t *testing.T) {
	// errors.Join means a thoroughly-broken config should surface
	// all the missing pieces in one report. Operators triaging
	// "why is my peer refusing to start?" deserve a complete
	// picture, not a fix-one-thing-find-the-next loop.
	err := ValidateAbuseRules(`{"route":{}}`)
	if err == nil {
		t.Fatal("expected error for empty route block")
	}
	msg := err.Error()
	for _, want := range []string{"abuse tags", "RFC1918", "SMTP"} {
		if !strings.Contains(msg, want) {
			t.Errorf("combined error should mention %q, got: %v", want, err)
		}
	}
}

func TestValidateAbuseRules_BadJSON(t *testing.T) {
	err := ValidateAbuseRules(`{not valid json`)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse launch_cfg JSON") {
		t.Errorf("error should mention JSON parse failure, got: %v", err)
	}
}

// sing-box accepts both `"rule_set": "tag"` (scalar) and
// `"rule_set": ["tag"]` (array) — the validator must too, otherwise
// a perfectly valid launch_cfg that happens to use the scalar form
// would be flagged as missing the tag.
func TestValidateAbuseRules_AcceptsScalarRuleSet(t *testing.T) {
	cfg := `{
		"route":{
			"rule_set":[
				{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
			],
			"rules":[
				{"action":"reject","rule_set":"geosite-malware"},
				{"action":"reject","rule_set":"geoip-malware"},
				{"action":"reject","rule_set":"geosite-phishing"},
				{"action":"reject","rule_set":"geosite-cryptominers"},
				{"action":"reject","ip_cidr":"10.0.0.0/8"},
				{"action":"reject","port":25}
			]
		}}`
	if err := ValidateAbuseRules(cfg); err != nil {
		t.Fatalf("scalar rule_set / ip_cidr / port should be valid, got: %v", err)
	}
}

// A reject rule with invert=true rejects everything EXCEPT the
// listed match — the opposite of what an abuse-block rule should do.
// It must not satisfy the abuse-tag check.
func TestValidateAbuseRules_RejectsInverted(t *testing.T) {
	cfg := `{
		"route":{
			"rule_set":[
				{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
			],
			"rules":[
				{"action":"reject","rule_set":["geosite-malware"],"invert":true},
				{"action":"reject","rule_set":["geoip-malware"]},
				{"action":"reject","rule_set":["geosite-phishing"]},
				{"action":"reject","rule_set":["geosite-cryptominers"]},
				{"action":"reject","ip_cidr":["10.0.0.0/8"]},
				{"action":"reject","port":[25]}
			]
		}}`
	err := ValidateAbuseRules(cfg)
	if err == nil {
		t.Fatal("inverted reject must not satisfy the abuse-tag check")
	}
	if !strings.Contains(err.Error(), "geosite-malware") {
		t.Errorf("error should call out the inverted tag, got: %v", err)
	}
}

// A reject rule that ANDs the abuse rule_set with another constraint
// (port, domain, source IP, etc.) only fires for the intersection —
// most abuse-list traffic still passes. Must not count as covering
// the tag.
func TestValidateAbuseRules_RejectsExtraConstraint(t *testing.T) {
	cfg := `{
		"route":{
			"rule_set":[
				{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
			],
			"rules":[
				{"action":"reject","rule_set":["geosite-malware"],"port":[80]},
				{"action":"reject","rule_set":["geoip-malware"]},
				{"action":"reject","rule_set":["geosite-phishing"]},
				{"action":"reject","rule_set":["geosite-cryptominers"]},
				{"action":"reject","ip_cidr":["10.0.0.0/8"]},
				{"action":"reject","port":[25]}
			]
		}}`
	err := ValidateAbuseRules(cfg)
	if err == nil {
		t.Fatal("reject with extra constraint must not satisfy the abuse-tag check")
	}
	if !strings.Contains(err.Error(), "geosite-malware") {
		t.Errorf("error should call out the constrained tag, got: %v", err)
	}
}

// Same predicate-narrowing concern applies to the static canary
// reject rules. An ip_cidr reject ANDed with a port no longer
// covers all RFC1918 traffic and must not count as the canary.
func TestValidateAbuseRules_RejectsStaticCanaryWithExtraConstraint(t *testing.T) {
	cfg := `{
		"route":{
			"rule_set":[
				{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
			],
			"rules":[
				{"action":"reject","rule_set":["geosite-malware"]},
				{"action":"reject","rule_set":["geoip-malware"]},
				{"action":"reject","rule_set":["geosite-phishing"]},
				{"action":"reject","rule_set":["geosite-cryptominers"]},
				{"action":"reject","ip_cidr":["10.0.0.0/8"],"port":[80]},
				{"action":"reject","port":[25]}
			]
		}}`
	err := ValidateAbuseRules(cfg)
	if err == nil {
		t.Fatal("RFC1918 canary ANDed with port must not satisfy the static-block check")
	}
	if !strings.Contains(err.Error(), "RFC1918") {
		t.Errorf("error should call out the missing RFC1918 canary, got: %v", err)
	}
}

// Explicit invert=false should be treated as the canonical no-op
// (sing-box's default is false) and still credit the rule.
func TestValidateAbuseRules_AcceptsExplicitInvertFalse(t *testing.T) {
	cfg := `{
		"route":{
			"rule_set":[
				{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
				{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
			],
			"rules":[
				{"action":"reject","rule_set":["geosite-malware"],"invert":false},
				{"action":"reject","rule_set":["geoip-malware"]},
				{"action":"reject","rule_set":["geosite-phishing"]},
				{"action":"reject","rule_set":["geosite-cryptominers"]},
				{"action":"reject","ip_cidr":["10.0.0.0/8"]},
				{"action":"reject","port":[25]}
			]
		}}`
	if err := ValidateAbuseRules(cfg); err != nil {
		t.Fatalf("explicit invert=false should be treated as a pure reject, got: %v", err)
	}
}

// Tolerating the discriminator must not extend to types that carry their own
// matching fields. A logical rule scopes via mode/rules, so it is not an
// unconditional reject even though it says action=reject.
func TestValidateAbuseRules_RejectsNonDefaultRuleType(t *testing.T) {
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","rule_set":["geosite-malware"]}`,
		`{"type":"logical","action":"reject","rule_set":["geosite-malware"]}`, 1)
	if bad == minimalValidLaunchCfg {
		t.Fatal("fixture substitution did not apply — test would be vacuous")
	}
	if err := ValidateAbuseRules(bad); err == nil {
		t.Fatal("a logical rule must not count as an unconditional reject")
	}
}

func TestValidateAbuseRulesOptionsNeedsRoute(t *testing.T) {
	err := ValidateAbuseRulesOptions(option.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "route")
}

// directExitRules puts rules, such as a resolve and a private-address reject, ahead of
// minimalValidLaunchCfg's rules, and a final route with udp_connect after them, as
// lantern-cloud's peer configs do.
func directExitRules(rules string) string {
	cfg := strings.Replace(minimalValidLaunchCfg, `"rules":[`, `"rules":[`+rules+`,`, 1)
	return strings.Replace(cfg, `6697]}`, `6697]},`+finalRoute, 1)
}

const (
	resolveRule         = `{"action":"resolve","server":"dns"}`
	privateRejectRule   = `{"action":"reject","ip_is_private":true}`
	finalRoute          = `{"action":"route","outbound":"direct","udp_connect":true}`
	volunteerDirectExit = `"services":[{"type":"unbounded","tag":"unbounded-volunteer","discovery_srv":"https://freddie.test",` +
		`"egress_addr":"wss://egress.test","stun_servers":["stun:192.0.2.1:3478"],"direct_exit":true}]`
)

func parseOptions(t *testing.T, cfg string) option.Options {
	t.Helper()
	opts, err := json.UnmarshalExtendedContext[option.Options](box.BaseContext(), []byte(cfg))
	require.NoError(t, err)
	return opts
}

func TestValidateDirectExitRules(t *testing.T) {
	for name, tc := range map[string]struct {
		rules string
		ok    bool
	}{
		"resolve then private reject":     {rules: resolveRule + `,` + privateRejectRule, ok: true},
		"resolve with no options":         {rules: `{"action":"resolve"},` + privateRejectRule, ok: true},
		"no resolve":                      {rules: privateRejectRule},
		"no private reject":               {rules: resolveRule},
		"private reject before resolve":   {rules: privateRejectRule + `,` + resolveRule},
		"conditional resolve":             {rules: `{"action":"resolve","domain_suffix":["example.com"]},` + privateRejectRule},
		"conditional private reject":      {rules: resolveRule + `,{"action":"reject","ip_is_private":true,"port":[80]}`},
		"inverted private reject":         {rules: resolveRule + `,{"action":"reject","ip_is_private":true,"invert":true}`},
		"private route, not reject":       {rules: resolveRule + `,{"action":"route","outbound":"direct","ip_is_private":true}`},
		"source private reject":           {rules: resolveRule + `,{"action":"reject","source_ip_is_private":true}`},
		"rules between resolve and check": {rules: resolveRule + `,{"action":"reject","port":[22]},` + privateRejectRule, ok: true},
		"unconditional route first":       {rules: finalRoute + `,` + resolveRule + `,` + privateRejectRule},
		"probe route first":               {rules: `{"action":"route","outbound":"probe","domain":["api.test"],"port":[80]},` + resolveRule + `,` + privateRejectRule, ok: true},
		"probe route by suffix":           {rules: `{"action":"route","outbound":"probe","domain_suffix":["api.test"]},` + resolveRule + `,` + privateRejectRule},
		"probe route by IP too":           {rules: `{"action":"route","outbound":"probe","domain":["api.test"],"ip_cidr":["192.0.2.0/24"]},` + resolveRule + `,` + privateRejectRule},
		"inverted probe route":            {rules: `{"action":"route","outbound":"probe","domain":["api.test"],"invert":true},` + resolveRule + `,` + privateRejectRule},
		"route before private reject":     {rules: resolveRule + `,{"action":"route","outbound":"direct","udp_connect":true,"ip_cidr":["127.0.0.1/32"]},` + privateRejectRule},
		"route-options first":             {rules: `{"action":"route-options","udp_connect":true},` + resolveRule + `,` + privateRejectRule},
		"hijack-dns first":                {rules: `{"action":"hijack-dns","port":[53]},` + resolveRule + `,` + privateRejectRule},
		"logical route first": {rules: `{"type":"logical","mode":"or","rules":[{"domain":["a.test"]},{"port":[80]}],` +
			`"action":"route","outbound":"direct","udp_connect":true},` + resolveRule + `,` + privateRejectRule},
		"sniff and rejects first": {rules: `{"action":"sniff","port":[80]},{"action":"reject","rule_set":["geosite-malware"],"invert":true},` +
			resolveRule + `,` + privateRejectRule, ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateDirectExitRulesOptions(parseOptions(t, directExitRules(tc.rules)))
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
	assert.Error(t, ValidateDirectExitRulesOptions(option.Options{}), "no route")
}

// lantern-cloud's peer configs, as cmd/api/pcfg builds them: peerExitOptions' abuse and
// static rejects, under applyPeerAllowlist (the pool's) or applyPersonalExit (a personal
// exit's), with injectBanditProbe's rules ahead of them. Its sniff comes only with endpoints.
const (
	cloudRuleSets = `"rule_set":[
		{"type":"inline","tag":"peer-allowlist","rules":[{"network":["tcp"],"domain":["allowed.test"],"port":[80,443]}]},
		{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
		{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
		{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
		{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
	]`
	cloudProbeSniff = `{"port":[80],"action":"sniff","sniffer":["http"]}`
	cloudProbe      = `{"domain":["api.test"],"port":[80],"action":"route","outbound":"banditprobe"}`
	cloudNotAllowed = `{"rule_set":["peer-allowlist"],"invert":true,"action":"reject"}`
	cloudAbuse      = `{"rule_set":["geosite-malware"],"action":"reject"},
		{"rule_set":["geoip-malware"],"action":"reject"},
		{"rule_set":["geosite-phishing"],"action":"reject"},
		{"rule_set":["geosite-cryptominers"],"action":"reject"},
		{"ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","127.0.0.0/8","169.254.0.0/16","::1/128","fc00::/7","fe80::/10"],"action":"reject"},
		{"port":[25,465,587,2525,6660,6661,6662,6663,6664,6665,6666,6667,6668,6669,6697],"action":"reject"}`
	cloudAllowed = `{"rule_set":["peer-allowlist"],"action":"route","outbound":"direct","udp_connect":true}`
)

func cloudConfig(rules ...string) string {
	return `{"route":{` + cloudRuleSets + `,"rules":[` + strings.Join(rules, ",") + `]}}`
}

func TestValidateDirectExitRulesTakesLanternCloudConfigs(t *testing.T) {
	pool := cloudConfig(cloudProbeSniff, cloudProbe, cloudNotAllowed, resolveRule, privateRejectRule, cloudAbuse, cloudAllowed, `{"action":"reject"}`)
	personal := cloudConfig(cloudProbe, resolveRule, privateRejectRule, cloudAbuse, finalRoute)
	for name, cfg := range map[string]string{
		"pool":                  pool,
		"personal":              personal,
		"personal, with sniff":  cloudConfig(cloudProbeSniff, cloudProbe, resolveRule, privateRejectRule, cloudAbuse, finalRoute),
		"personal, no probe":    cloudConfig(resolveRule, privateRejectRule, cloudAbuse, finalRoute),
		"pool, rejecting final": strings.Replace(pool, `]}}`, `],"final":"direct"}}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			opts := parseOptions(t, cfg)
			require.NoError(t, ValidateAbuseRulesOptions(opts))
			assert.NoError(t, ValidateDirectExitRulesOptions(opts))
		})
	}
}

// The rules must come in the order sing-box needs them: it stops at the first rule that routes
// a connection, and IP rules don't see a domain's addresses before a resolve. Every route
// pins a UDP flow to its first destination, which is all the rules check.
func TestValidateDirectExitRulesNeedsOrderAndUDPConnect(t *testing.T) {
	for name, cfg := range map[string]string{
		"unconditional route before the rejects": cloudConfig(finalRoute, resolveRule, privateRejectRule, cloudAbuse, finalRoute),
		"logical rule before the rejects": cloudConfig(`{"type":"logical","mode":"and","rules":[{"network":["tcp"]},{"port":[443]}],"action":"route","outbound":"direct","udp_connect":true}`,
			resolveRule, privateRejectRule, cloudAbuse, finalRoute),
		"abuse rejects only before the resolve":    cloudConfig(cloudAbuse, resolveRule, privateRejectRule, finalRoute),
		"allowlist route before the abuse rejects": cloudConfig(cloudNotAllowed, resolveRule, privateRejectRule, cloudAllowed, cloudAbuse, `{"action":"reject"}`),
		"route without udp_connect":                cloudConfig(cloudProbe, resolveRule, privateRejectRule, cloudAbuse, `{"action":"route","outbound":"direct"}`),
		"conditional route without udp_connect": cloudConfig(resolveRule, privateRejectRule, cloudAbuse,
			`{"rule_set":["peer-allowlist"],"action":"route","outbound":"direct"}`, `{"action":"reject"}`),
		"bypass without udp_connect":  cloudConfig(resolveRule, privateRejectRule, cloudAbuse, `{"action":"bypass","outbound":"direct","port":[443]}`, finalRoute),
		"direct action":               cloudConfig(resolveRule, privateRejectRule, cloudAbuse, `{"action":"direct","port":[443]}`, finalRoute),
		"final with no terminal rule": strings.Replace(cloudConfig(resolveRule, privateRejectRule, cloudAbuse, cloudAllowed), `]}}`, `],"final":"direct"}}`, 1),
		// With no final, sing-box sends what no rule matched to its first outbound.
		"no final and no terminal rule": cloudConfig(resolveRule, privateRejectRule, cloudAbuse, cloudAllowed),
		"conditional last rule":         cloudConfig(resolveRule, privateRejectRule, cloudAbuse, `{"action":"reject","port":[22]}`),
		"inverted last rule":            cloudConfig(resolveRule, privateRejectRule, cloudAbuse, `{"action":"reject","port":[22],"invert":true}`),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, ValidateDirectExitRulesOptions(parseOptions(t, cfg)))
		})
	}
}

func volunteerOptions(t *testing.T, opts option.Options) *LO.UnboundedVolunteerOptions {
	t.Helper()
	require.Len(t, opts.Services, 1)
	vol, ok := opts.Services[0].Options.(*LO.UnboundedVolunteerOptions)
	require.True(t, ok)
	return vol
}

// A volunteer exits strangers' traffic directly only under rules that check it: the abuse
// rules, and a resolve before a private-address reject so the IP rules see domains too.
func TestStripUnsafeDirectExit(t *testing.T) {
	withVolunteer := func(cfg string) string {
		return strings.Replace(cfg, `"inbounds":[{"type":"samizdat","tag":"samizdat-in"}],`, volunteerDirectExit+`,`, 1)
	}
	for name, tc := range map[string]struct {
		config   string
		stripped bool
	}{
		"every rule":       {config: withVolunteer(directExitRules(resolveRule + `,` + privateRejectRule))},
		"abuse rules only": {config: withVolunteer(minimalValidLaunchCfg), stripped: true},
		"no route":         {config: `{` + volunteerDirectExit + `}`, stripped: true},
		"no abuse rules":   {config: `{"route":{"rules":[` + resolveRule + `,` + privateRejectRule + `]},` + volunteerDirectExit + `}`, stripped: true},
	} {
		t.Run(name, func(t *testing.T) {
			opts := parseOptions(t, tc.config)
			assert.Equal(t, tc.stripped, StripUnsafeDirectExit(&opts))
			vol := volunteerOptions(t, opts)
			assert.Equal(t, !tc.stripped, vol.DirectExit)
			assert.Equal(t, "https://freddie.test", vol.DiscoverySrv, "the volunteer still relays")
		})
	}
	t.Run("no direct exit", func(t *testing.T) {
		opts := parseOptions(t, `{`+strings.Replace(volunteerDirectExit, `,"direct_exit":true`, ``, 1)+`}`)
		assert.False(t, StripUnsafeDirectExit(&opts))
		assert.False(t, volunteerOptions(t, opts).DirectExit)
	})
	t.Run("no volunteer", func(t *testing.T) {
		opts := parseOptions(t, `{}`)
		assert.False(t, StripUnsafeDirectExit(&opts))
	})
}

// CheckDirectExit strips as StripUnsafeDirectExit does, and marks the context only when a
// direct exit is left, so a box's volunteer runs one only from options checked for it.
func TestCheckDirectExit(t *testing.T) {
	withVolunteer := func(cfg string) string {
		return strings.Replace(cfg, `"inbounds":[{"type":"samizdat","tag":"samizdat-in"}],`, volunteerDirectExit+`,`, 1)
	}
	noExit := `{` + strings.Replace(volunteerDirectExit, `,"direct_exit":true`, ``, 1) + `}`
	assert.False(t, DirectExitChecked(context.Background()), "an unchecked context")
	for name, tc := range map[string]struct {
		config            string
		stripped, checked bool
	}{
		"every rule":       {config: withVolunteer(directExitRules(resolveRule + `,` + privateRejectRule)), checked: true},
		"abuse rules only": {config: withVolunteer(minimalValidLaunchCfg), stripped: true},
		"no direct exit":   {config: noExit},
		"no volunteer":     {config: `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			opts := parseOptions(t, tc.config)
			ctx, stripped := CheckDirectExit(context.Background(), &opts)
			assert.Equal(t, tc.stripped, stripped)
			assert.Equal(t, tc.checked, DirectExitChecked(ctx))
			if len(opts.Services) > 0 {
				assert.Equal(t, tc.checked, volunteerOptions(t, opts).DirectExit)
			}

			// A context checked for other options doesn't carry over.
			checkedOpts := parseOptions(t, withVolunteer(directExitRules(resolveRule+`,`+privateRejectRule)))
			marked, _ := CheckDirectExit(context.Background(), &checkedOpts)
			require.True(t, DirectExitChecked(marked))
			opts = parseOptions(t, tc.config)
			ctx, _ = CheckDirectExit(marked, &opts)
			assert.Equal(t, tc.checked, DirectExitChecked(ctx))
		})
	}
}
