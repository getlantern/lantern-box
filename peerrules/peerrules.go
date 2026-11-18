package peerrules

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	box "github.com/getlantern/lantern-box"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	C "github.com/getlantern/lantern-box/constant"
	LO "github.com/getlantern/lantern-box/option"
)

// abuseRuleSetTags is the canonical list of abuse rule_set tags that
// the peer launch_cfg MUST carry. Mirrors the server-side abuseTags
// list that emits the rule_set entries into the registration response.
// If the server-side list grows or renames a tag, this list grows
// with it — the server-side test asserts the emit side; this list
// asserts the client side sees the same shape after registration.
var abuseRuleSetTags = []string{
	"geosite-malware",
	"geoip-malware",
	"geosite-phishing",
	"geosite-cryptominers",
}

// rfc1918CanaryCIDR and smtpCanaryPort are sentinel values that, if
// missing from the launch_cfg's reject rules, indicate the server-
// side static peer-egress-block list was dropped or mutated. We pick
// one IP-CIDR and one port from each block as a low-cost smoke test;
// a full structural check would be brittle to upstream additions.
const (
	rfc1918CanaryCIDR = "10.0.0.0/8"
	smtpCanaryPort    = 25
)

// ValidateAbuseRules is ValidateAbuseRulesOptions on sing-box options in JSON, such as a peer
// registration's server_config.
func ValidateAbuseRules(optionsJSON string) error {
	ctx := box.BaseContext()
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(optionsJSON))
	if err != nil {
		return fmt.Errorf("parse launch_cfg JSON: %w", err)
	}
	return ValidateAbuseRulesOptions(options)
}

// ValidateAbuseRulesOptions is a defence-in-depth check on the options a peer registration
// returns. The server is supposed to embed a set of route.rule_set +
// route.rules entries that block the peer from forwarding traffic to
// known-malicious destinations, RFC1918 CIDRs, and abuse-prone ports.
//
// If a future server-side regression ships a launch_cfg without those
// rules, every newly-registered peer would silently turn into an open
// residential proxy until someone noticed. This validator blocks
// Start before sing-box runs an unsafe config; the peer prefers to fail
// to share at all rather than share unsafely.
//
// The check is structural-only — it confirms the expected rule_set
// tags appear in both route.rule_set and route.rules (as an
// unconditional reject), plus two canary entries from the static
// reject block. It does NOT verify the .srs files at the rule_set
// URLs are uncorrupted or that the URLs themselves are trustworthy;
// those are separate supply-chain concerns and are not in scope for
// this gate.
func ValidateAbuseRulesOptions(options option.Options) error {
	if options.Route == nil {
		return errors.New("launch_cfg is missing route block — peer would have no abuse blocking at all")
	}
	var errs []error
	if err := validateAbuseRuleSetTags(options.Route.RuleSet); err != nil {
		errs = append(errs, err)
	}
	if err := validateAbuseRejectRules(options.Route.Rules); err != nil {
		errs = append(errs, err)
	}
	if err := validateStaticRejectCanaries(options.Route.Rules); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidateDirectExitRulesOptions checks what a config that exits strangers' traffic directly
// needs on top of the abuse rules, in the order sing-box needs it: an unconditional resolve,
// then an unconditional reject of private addresses and the abuse and static rejects
// ValidateAbuseRulesOptions looks for. Before a resolve, sing-box's IP rules don't match a
// destination given as a domain, so a name that resolves into a private or listed range would
// pass them. sing-box stops at the first rule that routes a connection, so until all of those
// have come, a rule may only reject, sniff or resolve, or be lantern-cloud's bandit probe: a
// route by exact domain, and port, alone.
//
// Every route must also set udp_connect: hysteria2 names a destination per UDP datagram, and
// the rules see only a flow's first. Nothing may fall through to route.final or the default
// outbound, which can't set it, so the last rule must reject or route everything.
//
// Like ValidateAbuseRulesOptions, it is structural only, and its errors name no rule.
func ValidateDirectExitRulesOptions(options option.Options) error {
	if options.Route == nil {
		return errors.New("launch_cfg is missing route block")
	}
	rules := options.Route.Rules
	var resolved, privateRejected, rfc1918, smtp bool
	rejectedTags := map[string]bool{}
	guarded := func() bool {
		return privateRejected && rfc1918 && smtp && len(rejectedTags) == len(abuseRuleSetTags)
	}
	for _, r := range rules {
		action := ruleAction(r)
		if !guarded() {
			if !mayPrecedeSafeguards(r, action) {
				return errors.New("route.rules has a rule that can route traffic before its resolve, ip_is_private reject and abuse rejects")
			}
			if isDefaultRule(r) {
				d := r.DefaultOptions
				switch {
				case isUnconditionalResolve(d):
					resolved = true
				case !resolved:
				case d.IPIsPrivate && isUnconditionalReject(d, "ip_is_private"):
					privateRejected = true
				case isUnconditionalReject(d, "rule_set"):
					for _, tag := range d.RuleSet {
						if slices.Contains(abuseRuleSetTags, tag) {
							rejectedTags[tag] = true
						}
					}
				case isUnconditionalReject(d, "ip_cidr"):
					rfc1918 = rfc1918 || slices.Contains(d.IPCIDR, rfc1918CanaryCIDR)
				case isUnconditionalReject(d, "port"):
					smtp = smtp || slices.Contains(d.Port, smtpCanaryPort)
				}
			}
		}
		if !pinsUDP(r, action) {
			return errors.New("route.rules has a route without udp_connect (a UDP flow could change destination unchecked)")
		}
	}
	switch {
	case !resolved:
		return errors.New("route.rules has no unconditional resolve (IP rules would not see a domain's addresses)")
	case !privateRejected:
		return errors.New("route.rules has no unconditional ip_is_private reject after its resolve")
	case !guarded():
		return errors.New("route.rules has no abuse or static rejects after its resolve (IP rules before it would not see a domain's addresses)")
	case len(rules) == 0 || !isTerminal(rules[len(rules)-1]):
		return errors.New("route.rules does not end in an unconditional reject or route with udp_connect (traffic would fall through to an outbound without it)")
	}
	return nil
}

// isDefaultRule reports whether r is a default rule, which sing-box takes an empty type for.
func isDefaultRule(r option.Rule) bool {
	return r.Type == "" || r.Type == constant.RuleTypeDefault
}

// ruleAction is r's action, with sing-box's default of route for an empty one.
func ruleAction(r option.Rule) option.RuleAction {
	action := r.LogicalOptions.RuleAction
	if isDefaultRule(r) {
		action = r.DefaultOptions.RuleAction
	}
	if action.Action == "" {
		action.Action = constant.RuleActionTypeRoute
	}
	return action
}

// mayPrecedeSafeguards reports whether r may come before a direct exit's resolve and rejects:
// it rejects, sniffs or resolves, which never sends traffic out, or it is a bandit probe.
func mayPrecedeSafeguards(r option.Rule, action option.RuleAction) bool {
	switch action.Action {
	case constant.RuleActionTypeReject, constant.RuleActionTypeSniff, constant.RuleActionTypeResolve:
		return true
	}
	return isBanditProbe(r, action)
}

// isBanditProbe reports whether r is lantern-cloud's bandit probe rule: a route of exact
// domains, on any or given ports, with no other match field and no invert. lantern-cloud puts
// it first, ahead of the resolve, and it routes to an outbound that answers locally.
func isBanditProbe(r option.Rule, action option.RuleAction) bool {
	if !isDefaultRule(r) || action.Action != constant.RuleActionTypeRoute || len(r.DefaultOptions.Domain) == 0 {
		return false
	}
	return reflect.DeepEqual(r.DefaultOptions.RawDefaultRule, option.RawDefaultRule{
		Domain: r.DefaultOptions.Domain,
		Port:   r.DefaultOptions.Port,
	})
}

// pinsUDP reports whether r, if it sends traffic out, pins a UDP flow to the destination the
// rules checked. The bandit probe needn't: its outbound never dials.
func pinsUDP(r option.Rule, action option.RuleAction) bool {
	switch action.Action {
	case constant.RuleActionTypeRoute:
		return action.RouteOptions.UDPConnect || isBanditProbe(r, action)
	case constant.RuleActionTypeBypass:
		return action.BypassOptions.UDPConnect
	case constant.RuleActionTypeDirect:
		return false
	}
	return true
}

// isTerminal reports whether r takes every connection that reaches it: a default rule with no
// match field and no invert, which rejects, or routes with udp_connect.
func isTerminal(r option.Rule) bool {
	if !isDefaultRule(r) || !reflect.DeepEqual(r.DefaultOptions.RawDefaultRule, option.RawDefaultRule{}) {
		return false
	}
	action := ruleAction(r)
	return action.Action == constant.RuleActionTypeReject ||
		action.Action == constant.RuleActionTypeRoute && action.RouteOptions.UDPConnect
}

// StripUnsafeDirectExit turns off direct_exit on every Unbounded volunteer service in options
// unless options pass ValidateAbuseRulesOptions and ValidateDirectExitRulesOptions: a
// volunteer's direct exit routes strangers' traffic through its box's route rules. It reports
// whether it turned any off. The volunteer still relays.
func StripUnsafeDirectExit(options *option.Options) bool {
	var exits []*LO.UnboundedVolunteerOptions
	for _, svc := range options.Services {
		if vol, ok := svc.Options.(*LO.UnboundedVolunteerOptions); ok && svc.Type == C.TypeUnbounded && vol.DirectExit {
			exits = append(exits, vol)
		}
	}
	if len(exits) == 0 {
		return false
	}
	if ValidateAbuseRulesOptions(*options) == nil && ValidateDirectExitRulesOptions(*options) == nil {
		return false
	}
	for _, vol := range exits {
		vol.DirectExit = false
	}
	return true
}

// isUnconditionalResolve reports whether rule resolves every destination: a resolve action,
// whatever its options, with no match field and no invert.
func isUnconditionalResolve(rule option.DefaultRule) bool {
	return rule.Action == constant.RuleActionTypeResolve && reflect.DeepEqual(rule.RawDefaultRule, option.RawDefaultRule{})
}

// validateAbuseRuleSetTags asserts every entry in abuseRuleSetTags is
// declared in route.rule_set. Missing entries mean sing-box won't even
// download the abuse list, so no destination check ever happens.
func validateAbuseRuleSetTags(rulesets []option.RuleSet) error {
	missing := make([]string, 0, len(abuseRuleSetTags))
	for _, tag := range abuseRuleSetTags {
		if !slices.ContainsFunc(rulesets, func(rs option.RuleSet) bool { return slices.Contains(rs.Tag, tag) }) {
			missing = append(missing, tag)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("route.rule_set is missing abuse tags: %v (peer would not block matching destinations)", missing)
	}
	return nil
}

// validateAbuseRejectRules asserts every abuseRuleSetTags entry also
// has a matching reject rule in route.rules. A rule_set without a
// matching reject is a no-op — sing-box downloads the list and does
// nothing with it.
//
// Only counts *unconditional* rejects (see isUnconditionalReject):
// a reject rule with an extra match constraint (port, domain, source
// IP, etc.) or with invert=true would let traffic in the abuse list
// through under most conditions; counting it as covering the tag
// would mask a misconfigured launch_cfg.
func validateAbuseRejectRules(rules []option.Rule) error {
	rejectedTags := map[string]bool{}
	for _, r := range rules {
		if !isUnconditionalReject(r.DefaultOptions, "rule_set") {
			continue
		}
		for _, tag := range r.DefaultOptions.RuleSet {
			rejectedTags[tag] = true
		}
	}
	missing := make([]string, 0, len(abuseRuleSetTags))
	for _, want := range abuseRuleSetTags {
		if !rejectedTags[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("route.rules has no unconditional reject for abuse tags: %v (rule_sets would download but not unconditionally block)", missing)
	}
	return nil
}

// validateStaticRejectCanaries spot-checks that the static
// destination-based reject rules (RFC1918 CIDRs + abuse ports) are
// present. Picks one canary from each block rather than asserting the
// full set so legitimate additions to the rules don't break this
// check.
func validateStaticRejectCanaries(rules []option.Rule) error {
	gotRFC1918 := false
	gotSMTP := false
	for _, r := range rules {
		if isUnconditionalReject(r.DefaultOptions, "ip_cidr") {
			gotRFC1918 = gotRFC1918 || slices.ContainsFunc(r.DefaultOptions.IPCIDR,
				func(cidr string) bool { return cidr == rfc1918CanaryCIDR },
			)
		}
		if isUnconditionalReject(r.DefaultOptions, "port") {
			gotSMTP = gotSMTP || slices.ContainsFunc(r.DefaultOptions.Port,
				func(p uint16) bool { return p == smtpCanaryPort },
			)
		}
	}
	var missing []string
	if !gotRFC1918 {
		missing = append(missing, fmt.Sprintf("RFC1918 reject (canary %s)", rfc1918CanaryCIDR))
	}
	if !gotSMTP {
		missing = append(missing, fmt.Sprintf("SMTP-port reject (canary :%d)", smtpCanaryPort))
	}
	if len(missing) > 0 {
		return fmt.Errorf("route.rules is missing static abuse blocks: %v", missing)
	}
	return nil
}

// isUnconditionalReject reports whether rule is a plain reject scoped by
// onField alone — no other match field and no invert.
//
// It builds the exact rule that a bare reject-on-onField would produce
// (reject action, default method, only onField copied over) and compares
// the whole struct with reflect.DeepEqual. Any extra match field, an
// invert, or a non-default action/method leaves some other field non-zero,
// so the structs differ and the rule is not credited as covering the abuse
// class.
func isUnconditionalReject(rule option.DefaultRule, onField string) bool {
	var defaultValue option.DefaultRule
	defaultValue.RuleAction = option.RuleAction{
		Action: constant.RuleActionTypeReject,
		RejectOptions: option.RejectActionOptions{
			Method: constant.RuleActionRejectMethodDefault,
		},
	}
	switch onField {
	case "rule_set":
		defaultValue.RuleSet = rule.RuleSet
	case "ip_cidr":
		defaultValue.IPCIDR = rule.IPCIDR
	case "port":
		defaultValue.Port = rule.Port
	case "ip_is_private":
		defaultValue.IPIsPrivate = rule.IPIsPrivate
	default:
		return false
	}
	return reflect.DeepEqual(rule, defaultValue)
}
