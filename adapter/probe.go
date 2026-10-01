package adapter

import "context"

type probeKey struct{}

// ContextWithProbe marks dials made with ctx as probes: connections that test
// an outbound rather than carry user traffic.
func ContextWithProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeKey{}, true)
}

// IsProbe reports whether ctx was marked by [ContextWithProbe].
func IsProbe(ctx context.Context) bool {
	probe, _ := ctx.Value(probeKey{}).(bool)
	return probe
}
