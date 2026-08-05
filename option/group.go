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

	// SwitchToleranceMs is the delay improvement (in ms) a candidate must
	// beat the current selection by before the group switches. Default 200.
	SwitchToleranceMs uint32 `json:"switch_tolerance_ms,omitempty"`

	// ConsecutiveFailureLimit hard-demotes a member once probe failures
	// reach this count, or once user-traffic failures within
	// UserFailureWindowSeconds reach this count. Default 3.
	ConsecutiveFailureLimit uint32 `json:"consecutive_failure_limit,omitempty"`

	// SoftDemoteLimit soft-demotes a member once user-traffic failures
	// within UserFailureWindowSeconds reach this count. A soft-demoted
	// member loses to every clean peer but still beats hard-demoted
	// peers. Set lower for a fleet with few alternatives; higher for a
	// large pool where one transient stall shouldn't be enough to push
	// the active member behind every clean alternative. Default 2.
	SoftDemoteLimit uint32 `json:"soft_demote_limit,omitempty"`

	// UserFailureWindowSeconds is the sliding-window length used to
	// count user-traffic failures (dial errors and data-plane stalls)
	// for the demote rule. Failures older than this age out of the
	// window so a transient failure self-recovers without depending on
	// traffic being routed through the member. Default 300 (5 min).
	UserFailureWindowSeconds uint32 `json:"user_failure_window_seconds,omitempty"`

	// MaxPersistedAgeSeconds caps the age of persisted TagHistory
	// entries on hydrate. Entries with UpdatedAt older than this are
	// dropped — bandit-driven candidate sets typically rotate every
	// 60 s – 15 min, so a stale snapshot from a prior session would
	// describe a member that may no longer exist. Default 3600 (1 h).
	MaxPersistedAgeSeconds uint32 `json:"max_persisted_age_seconds,omitempty"`

	// BackgroundIntervalSeconds is the active cadence for the low-priority
	// probe cycle (when data has flowed recently — see
	// IdleThresholdSeconds). Default 180.
	BackgroundIntervalSeconds uint32 `json:"background_interval_seconds,omitempty"`

	// IdleIntervalSeconds is the cadence used when no data has flowed
	// through a wrapped data-plane conn for IdleThresholdSeconds.
	// Default 900 (15 min).
	IdleIntervalSeconds uint32 `json:"idle_interval_seconds,omitempty"`

	// IdleThresholdSeconds is the no-data-plane-activity window after
	// which the background probe cadence backs off from
	// BackgroundIntervalSeconds to IdleIntervalSeconds. Default 600
	// (10 min).
	IdleThresholdSeconds uint32 `json:"idle_threshold_seconds,omitempty"`

	// LadderTotalBudgetSeconds is the total budget the reconnection ladder
	// has before emitting the exhaustion signal. Default 10.
	LadderTotalBudgetSeconds uint32 `json:"ladder_total_budget_seconds,omitempty"`

	// LadderCooldownSeconds is the minimum time after a ladder run before
	// another ladder run can fire. Suppresses repeated full-fleet
	// re-probes when stalls or dial errors arrive in quick succession on
	// the same already-shuffled pool. Default 60.
	LadderCooldownSeconds uint32 `json:"ladder_cooldown_seconds,omitempty"`

	// DataPlaneIdleSeconds is the no-traffic threshold after which an
	// established and proven tunnel is treated as a data-plane stall.
	// Default 60.
	DataPlaneIdleSeconds uint32 `json:"data_plane_idle_seconds,omitempty"`

	// DataPlaneProvedReadBytes is the cumulative Read-bytes threshold a
	// single wrapped conn must cross before the data-plane stall timer
	// is allowed to fire. Before a conn is proven, it is treated as
	// "established but inactive" — a brand-new conn, a handshake-only
	// conn, or a keepalive-only conn isn't evidence of stalling and
	// other failure paths (dial errors, probe failures) catch the
	// actually-broken cases. Default 4096.
	DataPlaneProvedReadBytes uint32 `json:"data_plane_proved_read_bytes,omitempty"`
}
