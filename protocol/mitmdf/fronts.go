package mitmdf

import (
	"errors"
	"fmt"
	"strings"

	"github.com/getlantern/lantern-box/option"
)

// frontEntry is the runtime form of a configured fronts-table row. The
// names list has been lowercased + trimmed; matching is suffix-aware DNS
// equality (an SNI matches if it equals a name or has a name as a `.foo`
// suffix).
type frontEntry struct {
	names        []string
	frontedSNI   string
	verifySAN    []string // lowercased
	redirectAddr string   // "" means dial the user's destination
}

// fronts is an ordered list of frontEntry rows. lookup returns the first
// entry that claims the given SNI, or nil if none match. Order matters:
// place more-specific entries first.
type fronts []*frontEntry

// buildFronts parses the configured fronts table into normalized
// frontEntry rows. Returns a deny list of unique names referenced across
// all entries (used by the outbound to validate destination metadata).
func buildFronts(in []option.MITMDFFrontEntry) (fronts, error) {
	if len(in) == 0 {
		return nil, errors.New("mitmdf: fronts table is empty")
	}
	out := make(fronts, 0, len(in))
	for i, e := range in {
		if e.FrontedSNI == "" {
			return nil, fmt.Errorf("mitmdf: fronts[%d]: fronted_sni is required", i)
		}
		if len(e.Names) == 0 {
			return nil, fmt.Errorf("mitmdf: fronts[%d]: names list is empty", i)
		}
		entry := &frontEntry{
			frontedSNI:   strings.ToLower(strings.TrimSpace(e.FrontedSNI)),
			redirectAddr: strings.TrimSpace(e.RedirectAddr),
		}
		seen := make(map[string]struct{}, len(e.Names))
		for _, n := range e.Names {
			n = normalizeDNS(n)
			if n == "" {
				return nil, fmt.Errorf("mitmdf: fronts[%d]: names contains an empty entry", i)
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			entry.names = append(entry.names, n)
		}
		for _, s := range e.VerifySAN {
			if s = normalizeDNS(s); s != "" {
				entry.verifySAN = append(entry.verifySAN, s)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// lookup returns the first front entry that claims sni, or nil.
func (f fronts) lookup(sni string) *frontEntry {
	sni = normalizeDNS(sni)
	if sni == "" {
		return nil
	}
	for _, e := range f {
		if e.matches(sni) {
			return e
		}
	}
	return nil
}

// matches is true when sni equals a configured name or is a strict
// subdomain (has the configured name as a `.foo` suffix). Caller must
// pass an already-normalized sni.
func (e *frontEntry) matches(sni string) bool {
	for _, n := range e.names {
		if sni == n || strings.HasSuffix(sni, "."+n) {
			return true
		}
	}
	return false
}

// denyMatch reports whether sni is covered by any of the names in deny
// (same exact-or-suffix semantics as frontEntry.matches). Caller passes
// already-normalized sni; deny entries are normalized at build time.
func denyMatch(deny []string, sni string) bool {
	sni = normalizeDNS(sni)
	for _, d := range deny {
		if sni == d || strings.HasSuffix(sni, "."+d) {
			return true
		}
	}
	return false
}

// normalizeDeny lowercases + trims a configured deny list, drops empties
// and duplicates. Returns nil for an empty input.
func normalizeDeny(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, n := range in {
		n = normalizeDNS(n)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// normalizeDNS lowercases, trims surrounding whitespace, and strips a
// single trailing dot from a DNS name. Returns "" for non-names.
func normalizeDNS(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(s, ".")
}
