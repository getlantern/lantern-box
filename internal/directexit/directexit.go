// Package directexit marks a box's context as built from options whose direct exits peerrules
// checked. It is internal so that only peerrules can set the mark, and an embedder gets it only
// through peerrules.CheckDirectExit; the volunteer service, which peerrules can't import, reads
// it.
package directexit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

type checkedKey struct{}

// WithChecked returns ctx marked with whether its box may run the direct exits in its options.
func WithChecked(ctx context.Context, checked bool) context.Context {
	return context.WithValue(ctx, checkedKey{}, checked)
}

// Checked reports whether ctx is marked as allowing its box's direct exits.
func Checked(ctx context.Context) bool {
	checked, _ := ctx.Value(checkedKey{}).(bool)
	return checked
}

type validUntilKey struct{}

// WithValidUntil returns ctx carrying when its box's direct exits must stop: the end of the
// registration that vouched for them.
func WithValidUntil(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, validUntilKey{}, t)
}

// ValidUntil is the deadline WithValidUntil put in ctx, if any. A box without one, such as
// `lantern-box run`'s, which its operator manages, keeps its direct exits.
func ValidUntil(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(validUntilKey{}).(time.Time)
	return t, ok
}

// AbuseRuleSetTags are the abuse rule sets a direct exit's config must reject, which
// peerrules checks it for.
var AbuseRuleSetTags = []string{
	"geosite-malware",
	"geoip-malware",
	"geosite-phishing",
	"geosite-cryptominers",
}

// RuleSetLookup is the part of sing-box's adapter.Router that finds a rule set by tag.
type RuleSetLookup interface {
	RuleSet(tag string) (adapter.RuleSet, bool)
}

// ErrAbuseRuleSetsNotLoaded is a box whose abuse rule sets haven't all loaded.
var ErrAbuseRuleSetsNotLoaded = errors.New("the abuse rule sets haven't loaded")

// AbuseRuleSetsLoaded reports whether each of router's abuse rule sets holds rules. A remote
// rule set whose download fails at start is no error to sing-box: the box starts with it empty,
// so its rejects match nothing, and retries later. A later download, or a local file's reload,
// can also replace a set's rules with none. A direct exit must not serve while any set is
// empty, or its rejects would let everything through to its final route, so a set counts as
// loaded only while it holds a rule, whatever its kind. The error names only the tags.
func AbuseRuleSetsLoaded(router RuleSetLookup) error {
	var missing []string
	for _, tag := range AbuseRuleSetTags {
		set, ok := router.RuleSet(tag)
		if !ok || !ruleSetLoaded(set) {
			missing = append(missing, tag)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrAbuseRuleSetsNotLoaded, strings.Join(missing, ", "))
	}
	return nil
}

// ruleSetLoaded reports whether set holds rules: its metadata is built from them, and every
// rule sets one of its flags.
func ruleSetLoaded(set adapter.RuleSet) bool {
	return set.Metadata() != (adapter.RuleSetMetadata{})
}
