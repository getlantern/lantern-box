package adapter

import (
	"regexp"
	"strconv"
	"strings"
)

// HelperTagPrefix starts the tag lantern-cloud gives each of a paired
// inviter's helper outbounds: "helper-" + 12 hex (see HelperTag and
// HelperUnboundedTag in lantern-cloud's cmd/api/pairing/peersid.go). The tag
// is a pseudonym of the pairing, so clients must not export it in logs or
// telemetry; pass any string that may carry an outbound tag through
// RedactHelperTags first.
const HelperTagPrefix = "helper-"

var helperTagRE = regexp.MustCompile(`\b` + HelperTagPrefix + `[0-9a-f]{` + strconv.Itoa(helperTagHexLen) + `}\b`)

// helperTagHexLen is the number of lowercase hex digits after HelperTagPrefix.
const helperTagHexLen = 12

// IsHelperTag reports whether tag is, in full, a helper tag: HelperTagPrefix
// followed by exactly 12 lowercase hex digits.
func IsHelperTag(tag string) bool {
	hex, ok := strings.CutPrefix(tag, HelperTagPrefix)
	if !ok || len(hex) != helperTagHexLen {
		return false
	}
	for i := 0; i < len(hex); i++ {
		if c := hex[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RedactHelperTags replaces each helper tag in s with "helper". It is cheap
// when s has none, so it is safe on hot logging and tracing paths.
func RedactHelperTags(s string) string {
	if !strings.Contains(s, HelperTagPrefix) {
		return s
	}
	return helperTagRE.ReplaceAllLiteralString(s, "helper")
}
