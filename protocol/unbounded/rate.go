package unbounded

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ported from getlantern/lantern-headless-client#12 by Ilya Yakelzon (reflog).

// byteRate is a rate in bytes per second.
type byteRate uint64

// parseRate reads a rate such as "5mbit", "20Mbps", "500KB/s", "1.5MiB/s" or
// "2MB". The unit letter decides bits versus bytes the way network tooling
// does: a lowercase b, "bit" or "bps" means bits, an uppercase B or "byte"
// means bytes. k, M and G multiply by 1000; Ki, Mi and Gi by 1024. "0" and
// "" mean unlimited.
// ValidateBandwidthLimit reports whether s is a bandwidth_limit the volunteer
// service accepts, so a caller can refuse a bad one before building a box.
func ValidateBandwidthLimit(s string) error {
	_, err := parseRate(s)
	return err
}

func parseRate(s string) (byteRate, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	m := rateRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("unrecognized rate %q (examples: 5mbit, 500KB/s, 1.5MiB/s)", s)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("unrecognized rate %q", s)
	}
	mult := 1.0
	base := 1000.0
	if m[3] != "" {
		base = 1024
	}
	switch strings.ToLower(m[2]) {
	case "k":
		mult = base
	case "m":
		mult = base * base
	case "g":
		mult = base * base * base
	}
	bits := false
	switch m[4] {
	case "bit", "bits", "bps", "b":
		bits = true
	}
	bytesPerSec := value * mult
	if bits {
		bytesPerSec /= 8
	}
	if bytesPerSec < 1 {
		return 0, fmt.Errorf("rate %q is below 1 byte per second", s)
	}
	return byteRate(bytesPerSec), nil
}

// rateRe: number, optional k/m/g prefix, optional i for binary, unit, optional
// per-second suffix. The unit group is case-sensitive on purpose (b vs B).
var rateRe = regexp.MustCompile(`^([0-9]*\.?[0-9]+)\s*([kKmMgG]?)(i?)(bits?|bps|b|bytes?|B)(?:/s|ps)?$`)

func (r byteRate) String() string {
	if r == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%.1f Mbit/s", float64(r)*8/1e6)
}
