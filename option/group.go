package option

import "github.com/sagernet/sing/common/json/badoption"

type FallbackOutboundOptions struct {
	// Primary and Fallback are the tags of the primary and fallback outbounds.
	Primary  string `json:"primary"`
	Fallback string `json:"fallback"`
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

	// SwitchToleranceMs is the minimum delay improvement (in ms) a
	// candidate must have over the current selection before the group
	// switches on delay. Default 300.
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

	// IdleThresholdSeconds is how long the group may go without a dial,
	// listen, or data-plane traffic before background probing stops. The
	// next dial or listen restarts it, starting a full probe in the
	// background if the group was idle longer than BackgroundIntervalSeconds
	// or the network changed meanwhile. Default 900 (15 min).
	IdleThresholdSeconds uint32 `json:"idle_threshold_seconds,omitempty"`

	// LadderTotalBudgetSeconds is the total budget the reconnection ladder
	// has before emitting the exhaustion signal. Default 10.
	LadderTotalBudgetSeconds uint32 `json:"ladder_total_budget_seconds,omitempty"`

	// LadderCooldownSeconds is the minimum time after a ladder run before
	// another can fire. A ladder runs when a dial or listen fails on the
	// selected member and on its fast-failover alternate, or has no
	// alternate. Default 60.
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

	// ProbeConcurrency caps parallel member probes per batch. Default 6.
	ProbeConcurrency uint32 `json:"probe_concurrency,omitempty"`
}
