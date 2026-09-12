package datacap

import (
	"net/netip"
	"strings"
)

// TrafficUsage carries only coarse counters to the sidecar. A bidirectional
// connection is evidence of exchanged bytes, not application success or humanity.
type TrafficUsage struct {
	LanternBytes       int64 `json:"lanternBytes,omitempty"`
	ProbeBytes         int64 `json:"probeBytes,omitempty"`
	OtherBytes         int64 `json:"otherBytes,omitempty"`
	UnknownBytes       int64 `json:"unknownBytes,omitempty"`
	LanternConnections int64 `json:"lanternConnections,omitempty"`
	ProbeConnections   int64 `json:"probeConnections,omitempty"`
	OtherConnections   int64 `json:"otherConnections,omitempty"`
	UnknownConnections int64 `json:"unknownConnections,omitempty"`
}

func trafficReport(category string, bytes int64, bidirectional bool) *TrafficUsage {
	if category == "" {
		return nil
	}
	u := &TrafficUsage{}
	var count int64
	if bidirectional {
		count = 1
	}
	switch category {
	case "lantern":
		u.LanternBytes, u.LanternConnections = bytes, count
	case "probe":
		u.ProbeBytes, u.ProbeConnections = bytes, count
	case "other":
		u.OtherBytes, u.OtherConnections = bytes, count
	default:
		u.UnknownBytes, u.UnknownConnections = bytes, count
	}
	return u
}

// Only the requested hostname is examined, locally. This never resolves DNS,
// inspects TLS/HTTP contents, or retains destinations in reports or labels.
// Exact host matches deliberately avoid classifying unrelated subdomains.
func newTrafficClassifier(extraLantern, extraProbes []string) func(string) string {
	lantern := map[string]bool{}
	probes := map[string]bool{}
	for _, host := range append([]string{"api.iantem.io", "df.iantem.io", "api.getiantem.org", "api.lantern.io", "api.lantr.net"}, extraLantern...) {
		lantern[normalizeHost(host)] = true
	}
	for _, host := range append([]string{"www.gstatic.com", "connectivitycheck.gstatic.com", "connectivitycheck.android.com", "captive.apple.com", "www.msftconnecttest.com", "dns.msftncsi.com", "cp.cloudflare.com"}, extraProbes...) {
		probes[normalizeHost(host)] = true
	}
	return func(host string) string {
		host = normalizeHost(host)
		if host == "" {
			return "unknown"
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return "unknown"
		}
		if strings.ContainsAny(host, ":/[]@ \\?\t\r\n") {
			return "unknown"
		}
		if lantern[host] {
			return "lantern"
		}
		if probes[host] {
			return "probe"
		}
		return "other"
	}
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
