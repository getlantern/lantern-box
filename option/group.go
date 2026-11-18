package option

import (
	"encoding/json"

	"github.com/sagernet/sing/common/json/badoption"
)

type FallbackOutboundOptions struct {
	// Primary and Fallback are the tags of the primary and fallback outbounds.
	Primary  string `json:"primary"`
	Fallback string `json:"fallback"`
}

// HedgeOutboundOptions configure a hedge: each connection goes to First, and
// moves to Then, replaying what the client sent, if First gives no reply.
// Only a connection that opens with a TLS or QUIC handshake is replayed; any
// other stays on First.
// Whether First worked is remembered per site, so a site First can't reach
// goes straight to Then until FailureTTL passes.
type HedgeOutboundOptions struct {
	// First and Then are outbound tags. First is typically a proxyless
	// outbound, Then the proxy group. Bandit probes always go to Then.
	First string `json:"first"`
	Then  string `json:"then"`
	// CommitTimeout is how long First may take to send its first byte after
	// the client's first write before the connection moves to Then. It also
	// bounds First's dial; a dial that runs out of time goes to Then without
	// marking the site bad. Default 1s.
	CommitTimeout badoption.Duration `json:"commit_timeout,omitempty"`
	// FailureTTL is how long a site First failed for skips First. Default
	// 5m, longer than the GFW's 180s residual block.
	FailureTTL badoption.Duration `json:"failure_ttl,omitempty"`
	// SuccessTTL is how long a site First worked for is remembered as good,
	// which keeps the address-block prior below from sending it to Then.
	// Default 1h.
	SuccessTTL badoption.Duration `json:"success_ttl,omitempty"`
	// Disabled starts the hedge switched off: every connection goes straight
	// to Then until it is switched on through adapter.Switchable.
	Disabled bool `json:"disabled,omitempty"`
	// MaxHeldBytes bounds what is kept for replay before First replies, and
	// UDP also keeps at most 64 datagrams. A connection that writes more
	// can't move: if First fails or gives no reply within CommitTimeout, it
	// is closed and its site marked bad, so the client's retry uses Then.
	// Default 64 KiB.
	MaxHeldBytes int `json:"max_held_bytes,omitempty"`
}

type MutableSelectorOutboundOptions struct {
	Outbounds []string `json:"outbounds"`
}

type MutableURLTestOutboundOptions struct {
	Outbounds    []string           `json:"outbounds"`
	URL          string             `json:"url,omitempty"`
	URLOverrides map[string]string  `json:"url_overrides,omitempty"`
	Interval     badoption.Duration `json:"interval,omitempty"`
	Tolerance    uint16             `json:"tolerance,omitempty"`
	IdleTimeout  badoption.Duration `json:"idle_timeout,omitempty"`
}

// MutableAutoSelectOutboundOptions configures the MutableAutoSelect
// group. Zero values fall back to documented defaults.
type MutableAutoSelectOutboundOptions struct {
	Outbounds    []string          `json:"outbounds"`
	URL          string            `json:"url,omitempty"`
	URLOverrides map[string]string `json:"url_overrides,omitempty"`

	// ProbeTimeoutsMs overrides the probe timeout, in milliseconds, for
	// members of the given outbound types, e.g. {"hysteria2x": 5000}. A type
	// not listed, or a value outside 250 to 60000, keeps its built-in
	// timeout. SetProbeTimeouts replaces the set at runtime.
	ProbeTimeoutsMs ProbeTimeoutsMs `json:"probe_timeouts_ms,omitempty"`

	// SwitchToleranceMs is the minimum delay improvement (in ms) a
	// candidate must have over the current selection before the group
	// switches on delay. Default 100.
	SwitchToleranceMs uint32 `json:"switch_tolerance_ms,omitempty"`

	// SwitchTolerancePercent is the minimum delay improvement, as a
	// percentage of the current selection's delay, a candidate must also
	// have before the group switches on delay. Nil uses the default of 30;
	// 0 disables the check. Values over 100 are rejected.
	SwitchTolerancePercent *uint32 `json:"switch_tolerance_percent,omitempty"`

	// ConsecutiveFailureLimit hard-demotes a member once probe failures
	// reach this count, or once user-traffic failures within
	// UserFailureWindowSeconds reach this count. Default 3.
	ConsecutiveFailureLimit uint32 `json:"consecutive_failure_limit,omitempty"`

	// SoftDemoteLimit soft-demotes a member once user-traffic failures
	// within UserFailureWindowSeconds reach this count. Among eligible
	// members, soft-demoted peers rank below clean peers and above
	// hard-demoted peers. Set lower for a fleet with few alternatives;
	// higher for a large pool where one transient stall shouldn't be
	// enough to push the active member behind every clean alternative.
	// Default 2.
	SoftDemoteLimit uint32 `json:"soft_demote_limit,omitempty"`

	// UserFailureWindowSeconds is the sliding-window length used to
	// count user-traffic failures (dial errors, resets, unanswered
	// writes, and stalls) for the demote rule. Failures older than
	// this age out of the window so a transient failure self-recovers
	// without depending on traffic being routed through the member.
	// Default 300 (5 min).
	UserFailureWindowSeconds uint32 `json:"user_failure_window_seconds,omitempty"`

	// UserFailureDedupeWindowSeconds drops a user-traffic failure that
	// arrives within this long of the member's previous recorded failure,
	// so a burst counts once toward the demote rule. Default 45.
	UserFailureDedupeWindowSeconds uint32 `json:"user_failure_dedupe_window_seconds,omitempty"`

	// MaxPersistedAgeSeconds caps the age of persisted TagHistory
	// entries on hydrate. Entries with UpdatedAt older than this are
	// dropped — bandit-driven candidate sets typically rotate every
	// 60 s – 15 min, so a stale snapshot from a prior session would
	// describe a member that may no longer exist. Default 900 (15 min).
	MaxPersistedAgeSeconds uint32 `json:"max_persisted_age_seconds,omitempty"`

	// BackgroundIntervalSeconds is the background probe cadence while the
	// group is in use. Default 180.
	BackgroundIntervalSeconds uint32 `json:"background_interval_seconds,omitempty"`

	// Deprecated: IdleIntervalSeconds is ignored; background probing stops
	// after IdleThresholdSeconds without use instead of slowing down.
	IdleIntervalSeconds uint32 `json:"idle_interval_seconds,omitempty"`

	// IdleThresholdSeconds bounds inactivity before periodic probing stops.
	// The next use immediately requests probes for members without a fresh
	// outcome. Default 900.
	IdleThresholdSeconds uint32 `json:"idle_threshold_seconds,omitempty"`

	// LadderTotalBudgetSeconds bounds a ladder evaluation once its first
	// slot is available. Expiration leaves probes running. Default 15.
	LadderTotalBudgetSeconds uint32 `json:"ladder_total_budget_seconds,omitempty"`

	// LadderCooldownSeconds is the minimum time after a ladder run before
	// another can fire. A ladder runs when a dial or listen fails on the
	// selected member and on its fast-failover alternate, or has no
	// alternate. An interface update lifts the cooldown unless another
	// arrived within the previous LadderCooldownSeconds. Default 60.
	LadderCooldownSeconds uint32 `json:"ladder_cooldown_seconds,omitempty"`

	// ConfirmedFailureSwitchCooldownSeconds is the minimum time between
	// switches caused by failed confirmation probes, except when the
	// member selected by the last such switch also fails confirmation.
	// Default 60.
	ConfirmedFailureSwitchCooldownSeconds uint32 `json:"confirmed_failure_switch_cooldown_seconds,omitempty"`

	// DataPlaneIdleSeconds is the no-traffic threshold after which an
	// established and proven tunnel is treated as a data-plane stall.
	// Default 60.
	DataPlaneIdleSeconds uint32 `json:"data_plane_idle_seconds,omitempty"`

	// DataPlaneProvedReadBytes is the cumulative Read-bytes threshold a
	// single wrapped conn must cross before the data-plane stall timer
	// is allowed to fire. Default 4096.
	DataPlaneProvedReadBytes uint32 `json:"data_plane_proved_read_bytes,omitempty"`

	// DemoteOnlySelectedTag restricts data-plane stall, reset, and no-response
	// attribution to the tag currently selected for TCP or UDP. Default
	// true; set false to count failures from every wrapped conn.
	DemoteOnlySelectedTag *bool `json:"demote_only_selected_tag,omitempty"`

	// ProbeConcurrency caps concurrent probes of ordinary members across the
	// group; last-resort and failure-confirmation probes don't count toward
	// it. Default 6.
	ProbeConcurrency uint32 `json:"probe_concurrency,omitempty"`
}

// ProbeTimeoutsMs maps an outbound type to a probe timeout in milliseconds.
// Decoding it never fails: the value comes from a server, and an entry the
// client can't use must cost only that override, not the whole config.
// Entries that aren't a plain integer in uint32 range are dropped, including
// fractions and exponent forms such as 2.5, 3000.0 and 3e3, and so is a value
// that isn't an object.
type ProbeTimeoutsMs map[string]uint32

func (p *ProbeTimeoutsMs) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		*p = nil
		return nil
	}
	out := make(ProbeTimeoutsMs, len(raw))
	for typ, v := range raw {
		// Decoding into uint32 checks the JSON number's text exactly, so a
		// fraction can't round to a whole number on the way in.
		var ms *uint32
		if json.Unmarshal(v, &ms) != nil || ms == nil {
			continue
		}
		out[typ] = *ms
	}
	*p = out
	return nil
}
