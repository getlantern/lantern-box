package adapter

import (
	"regexp"
	"strings"
)

// HelperTagPrefix starts the tag lantern-cloud gives each of a paired
// inviter's helper outbounds: "helper-" + 12 hex (see HelperTag and
// HelperUnboundedTag in lantern-cloud's cmd/api/pairing/peersid.go). The tag
// is a pseudonym of the pairing, so clients must not export it in logs or
// telemetry; pass any string that may carry an outbound tag through
// RedactHelperTags first.
const HelperTagPrefix = "helper-"

var helperTagRE = regexp.MustCompile(`\b` + HelperTagPrefix + `[0-9a-f]{12}\b`)

// RedactHelperTags replaces each helper tag in s with "helper". It is cheap
// when s has none, so it is safe on hot logging and tracing paths.
func RedactHelperTags(s string) string {
	if !strings.Contains(s, HelperTagPrefix) {
		return s
	}
	return helperTagRE.ReplaceAllLiteralString(s, "helper")
}
