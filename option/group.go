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
	// beat the current selection by before the group switches. Default 50.
	SwitchToleranceMs uint32 `json:"switch_tolerance_ms,omitempty"`

	// HistoryRingSize caps the per-server outcome ring. Default 20.
	HistoryRingSize uint32 `json:"history_ring_size,omitempty"`

	// SuccessRateWindowMinutes is the look-back window used to compute
	// recent_success_rate for the demotion filter. Default 60.
	SuccessRateWindowMinutes uint32 `json:"success_rate_window_minutes,omitempty"`

	// DemoteSuccessRateBelow is the success-rate threshold under which a
	// server is demoted, provided it also has at least DemoteMinOutcomes
	// outcomes in the window. Default 0.3.
	DemoteSuccessRateBelow float64 `json:"demote_success_rate_below,omitempty"`

	// DemoteMinOutcomes is the minimum number of outcomes within the
	// success-rate window required to consider the rate signal at all.
	// Default 5.
	DemoteMinOutcomes uint32 `json:"demote_min_outcomes,omitempty"`

	// ConsecutiveFailureLimit bounds both the consecutive probe-failure
	// count and the user-failure count; reaching it on either counter
	// unconditionally demotes the server. Default 3.
	ConsecutiveFailureLimit uint32 `json:"consecutive_failure_limit,omitempty"`

	// BackgroundIntervalSeconds is the active cadence for the low-priority
	// probe cycle (when data has flowed recently — see
	// IdleThresholdSeconds). Default 180.
	BackgroundIntervalSeconds uint32 `json:"background_interval_seconds,omitempty"`

	// IdleIntervalSeconds is the cadence used when no data has flowed
	// through a wrapped data-plane conn for IdleThresholdSeconds. The
	// slower cadence trims background traffic on tunnels left idle.
	// Default 900.
	IdleIntervalSeconds uint32 `json:"idle_interval_seconds,omitempty"`

	// IdleThresholdSeconds is the no-data-plane-activity window after
	// which the background probe cadence backs off from
	// BackgroundIntervalSeconds to IdleIntervalSeconds. Default 600.
	IdleThresholdSeconds uint32 `json:"idle_threshold_seconds,omitempty"`

	// LadderFastRetryMs is the budget for the first step of the
	// reconnection ladder (re-probe the current member). Default 1000.
	LadderFastRetryMs uint32 `json:"ladder_fast_retry_ms,omitempty"`

	// LadderTotalBudgetSeconds is the total budget the reconnection ladder
	// has before emitting the exhaustion signal. Default 10.
	LadderTotalBudgetSeconds uint32 `json:"ladder_total_budget_seconds,omitempty"`

	// DataPlaneIdleSeconds is the no-traffic threshold after which an
	// established tunnel is treated as a data-plane failure. Default 30.
	DataPlaneIdleSeconds uint32 `json:"data_plane_idle_seconds,omitempty"`
}
