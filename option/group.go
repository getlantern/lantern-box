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
