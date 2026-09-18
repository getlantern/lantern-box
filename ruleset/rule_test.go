package ruleset

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

func TestMutableRuleSet(t *testing.T) {
	rsTag := "rule-set"
	domain := "ipconfig.io"

	ctx, instance, path := setup(t, rsTag, domain)
	defer os.RemoveAll(path)

	rs, loaded := instance.Router().RuleSet(rsTag)
	require.True(t, loaded, "ruleset not loaded")
	rs.StartContext(ctx, nil)

	// start the router rule. this would normally be done by the router itself when it starts
	rules := instance.Router().Rules()
	rsRule := rules[0]
	rsRule.Start()

	inboundCtx := func(domain string) *adapter.InboundContext {
		return &adapter.InboundContext{
			Domain: domain,
		}
	}

	m, _ := newMutableRuleSet(path, rsTag, "source", true)
	testStart(t, ctx, m, rsTag, domain)

	const added = "google.com"
	reloads := observeReloads(t, m, domain, added)

	reset := func(t *testing.T) {
		m.filterMu.Lock()
		m.filter.Domain = []string{domain}
		m.filterMu.Unlock()
		require.NoError(t, m.saveToFile())
		m.Enable()
		awaitReload(t, reloads, func(rs reloadState) bool { return rs[domain] && !rs[added] })
	}

	matchTests := []struct {
		name    string
		alterFn func(*testing.T, *MutableRuleSet) *adapter.InboundContext
		want    bool
	}{
		{
			name: "disable",
			alterFn: func(_ *testing.T, mrs *MutableRuleSet) *adapter.InboundContext {
				mrs.Disable()
				return inboundCtx(domain)
			},
			want: false,
		},
		{
			name: "re-enable",
			alterFn: func(_ *testing.T, mrs *MutableRuleSet) *adapter.InboundContext {
				mrs.Disable()
				mrs.Enable()
				return inboundCtx(domain)
			},
			want: true,
		},
		{
			name: "match added item",
			alterFn: func(t *testing.T, mrs *MutableRuleSet) *adapter.InboundContext {
				require.NoError(t, mrs.AddItem(TypeDomain, added))
				awaitReload(t, reloads, func(rs reloadState) bool { return rs[added] })
				return inboundCtx(added)
			},
			want: true,
		},
		{
			name: "should not match removed item",
			alterFn: func(t *testing.T, mrs *MutableRuleSet) *adapter.InboundContext {
				require.NoError(t, mrs.AddItem(TypeDomain, added))
				awaitReload(t, reloads, func(rs reloadState) bool { return rs[added] })
				require.NoError(t, mrs.RemoveItem(TypeDomain, domain))
				awaitReload(t, reloads, func(rs reloadState) bool { return !rs[domain] })
				return inboundCtx(domain)
			},
			want: false,
		},
	}
	for _, tt := range matchTests {
		t.Run(tt.name, func(t *testing.T) {
			reset(t)
			testMatch(t, instance, m, tt.alterFn, inboundCtx(domain), tt.want)
		})
	}
}

// reloadState records, for one reload of the watched rule set, whether each domain of
// interest matched.
type reloadState map[string]bool

// observeReloads registers a rule-set callback that evaluates the given domains against the
// freshly reloaded rules and publishes the result. The evaluation happens on the reloading
// goroutine, so it observes exactly the rules that reload installed and never races with
// the next one.
//
// The rule file is picked up by a debounced watcher: writes within ~100ms of each other
// collapse into one reload, writes further apart each get their own. A test that waits for
// "a reload" after each write therefore cannot know which write the reload it saw reflects,
// which is what made this test flake on slow CI runners. Waiting for the reload that shows
// the expected state removes the ambiguity.
func observeReloads(t *testing.T, mrs *MutableRuleSet, domains ...string) <-chan reloadState {
	t.Helper()
	reloads := make(chan reloadState, 64)
	cb := mrs.ruleset.RegisterCallback(func(rs adapter.RuleSet) {
		state := make(reloadState, len(domains))
		for _, d := range domains {
			state[d] = rs.Match(&adapter.InboundContext{Domain: d})
		}
		select {
		case reloads <- state:
		default:
			t.Error("reload observations not consumed")
		}
	})
	t.Cleanup(func() { mrs.ruleset.UnregisterCallback(cb) })
	return reloads
}

// awaitReload consumes reload observations until one satisfies want. Every caller has just
// written the rule file, so at least one reload is guaranteed to arrive.
func awaitReload(t *testing.T, reloads <-chan reloadState, want func(reloadState) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case state := <-reloads:
			if want(state) {
				return
			}
		case <-timeout:
			t.Fatal("rule set never reloaded into the expected state")
		}
	}
}

func setup(t *testing.T, rsTag, domain string) (context.Context, *sbox.Box, string) {
	path, err := os.MkdirTemp("", "test")
	require.NoError(t, err)
	rsFile := filepath.Join(path, rsTag+".json")
	err = os.WriteFile(rsFile, []byte(`{"version":3,"rules":[{"domain":"`+domain+`"}]}`), 0644)
	require.NoError(t, err, "failed to create rule file")

	ctx := box.BaseContext()

	instance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: testOptions(rsTag, rsFile),
	})
	require.NoError(t, err, "failed to create box instance")
	return ctx, instance, path
}

func testStart(t *testing.T, ctx context.Context, mRuleSet *MutableRuleSet, rsTag, domain string) {
	require.NoError(t, mRuleSet.Start(ctx), "Start failed")
	require.Len(t, mRuleSet.rules, 1, "rules not loaded")

	rule := mRuleSet.rules[0].(*ruleWrapper)
	require.Equal(t, rule.name, rsTag, "rule name mismatch")
	require.Contains(t, mRuleSet.filter.Domain, domain, "rule not loaded")
}

func testMatch(
	t *testing.T,
	instance *sbox.Box,
	mrs *MutableRuleSet,
	alter func(*testing.T, *MutableRuleSet) *adapter.InboundContext,
	inboundCtx *adapter.InboundContext,
	matchAltered bool,
) {
	router := instance.Router()
	rules := router.Rules()
	require.Len(t, rules, 1, "rules not loaded")
	assert.True(t, rules[0].Match(inboundCtx), "original rule match failed")

	ruleOriginal := rules[0].String()
	rsOriginal := mrs.ruleset.String()

	alterInboundCtx := alter(t, mrs)
	router = instance.Router()
	rules = router.Rules()
	require.Len(t, rules, 1, "rules not loaded")

	ruleAltered := rules[0].String()
	rsAltered := mrs.ruleset.String()
	fmtErr := func() string {
		return fmt.Sprintf("rule:\n\toriginal[%v]\n\taltered[%v]", ruleOriginal, ruleAltered) +
			fmt.Sprintf("\nruleset:\n\toriginal[%v]\n\taltered[%v]", rsOriginal, rsAltered) +
			fmt.Sprintf("\ninbound domain:\n\toriginal[%v]\n\taltered[%v]",
				inboundCtx.Domain, alterInboundCtx.Domain,
			)
	}
	assert.Equalf(t, matchAltered, rules[0].Match(alterInboundCtx), "altered rule match failed\n"+fmtErr())
}

func TestAddRemoveItems(t *testing.T) {
	path, err := os.MkdirTemp("", "test")
	require.NoError(t, err)
	defer os.RemoveAll(path)

	m, _ := newMutableRuleSet(path, "test", "source", false)
	reset := func() {
		m.filter = option.DefaultHeadlessRule{
			Domain:      []string{"test.com", "example.com"},
			ProcessName: []string{"chrome"},
		}
	}
	tests := []struct {
		name    string
		alterFn func(*MutableRuleSet)
		want    option.DefaultHeadlessRule
	}{
		{
			name: "add single item",
			alterFn: func(m *MutableRuleSet) {
				m.AddItem(TypeDomain, "google.com")
			},
			want: option.DefaultHeadlessRule{
				Domain:      []string{"test.com", "example.com", "google.com"},
				ProcessName: []string{"chrome"},
			},
		},
		{
			name: "remove single item",
			alterFn: func(m *MutableRuleSet) {
				m.RemoveItem(TypeDomain, "example.com")
			},
			want: option.DefaultHeadlessRule{
				Domain:      []string{"test.com"},
				ProcessName: []string{"chrome"},
			},
		},
		{
			name: "add multiple items",
			alterFn: func(m *MutableRuleSet) {
				m.AddItems(option.DefaultHeadlessRule{
					Domain:          []string{"google.com", "github.com"},
					DomainSuffix:    []string{".cn"},
					SourcePortRange: []string{"1000-2000"}, // not supported by the filter so should be ignored
				})
			},
			want: option.DefaultHeadlessRule{
				Domain:       []string{"test.com", "example.com", "google.com", "github.com"},
				DomainSuffix: []string{".cn"},
				ProcessName:  []string{"chrome"},
			},
		},
		{
			name: "remove multiple items",
			alterFn: func(m *MutableRuleSet) {
				m.RemoveItems(option.DefaultHeadlessRule{
					Domain:          []string{"google.com", "example.com"},
					ProcessName:     []string{"chrome"},
					SourcePortRange: []string{"1000-2000"}, // not supported by the filter so should be ignored
				})
			},
			want: option.DefaultHeadlessRule{
				Domain:      []string{"test.com"},
				ProcessName: []string{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset()
			tt.alterFn(m)
			assert.Equal(t, tt.want, m.filter)
		})
	}
}

func testOptions(rsTag, rsPath string) option.Options {
	opts := option.Options{
		Log: &option.LogOptions{
			Disabled: false,
			Output:   "stdout",
		},
		Inbounds: []option.Inbound{
			{
				Type: constant.TypeHTTP,
				Tag:  "http-in",
				Options: &option.HTTPMixedInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     common.Ptr(badoption.Addr(netip.MustParseAddr("127.0.0.1"))),
						ListenPort: 3003,
					},
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type: constant.TypeDirect,
			},
			{
				Type: constant.TypeHTTP,
				Tag:  "http-out",
				Options: &option.HTTPOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: 3000,
					},
				},
			},
		},
		Route: &option.RouteOptions{
			Rules: []option.Rule{
				BaseRouteRule(rsTag, "http-out"),
			},
			RuleSet: []option.RuleSet{
				LocalRuleSet(rsTag, rsPath, constant.RuleSetFormatSource),
			},
		},
	}
	return opts
}
