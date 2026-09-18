package adapter

// OutboundEvalConfig is the part of the outbound evaluation service's
// configuration that changes while the box runs. An empty Token idles the
// service, which is how a host with no credential yet leaves it.
type OutboundEvalConfig struct {
	Token       string
	CountryCode string
	OutboundTag string
}

// OutboundEvalConfigSetter is implemented by the outbound evaluation service,
// whose credential, market and outbound under test change while the box runs.
// It is declared here rather than in the implementing package so a host holding
// only an adapter.Service can reach it.
//
// The configuration is replaced whole, and takes effect on the next measurement
// cycle; one already in flight runs to completion under the configuration it
// started with.
type OutboundEvalConfigSetter interface {
	SetOutboundEvalConfig(config OutboundEvalConfig) error
}
