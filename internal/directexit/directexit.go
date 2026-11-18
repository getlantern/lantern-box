// Package directexit marks a box's context as built from options whose direct exits peerrules
// checked. It is internal so that only peerrules can set the mark, and an embedder gets it only
// through peerrules.CheckDirectExit; the volunteer service, which peerrules can't import, reads
// it.
package directexit

import (
	"context"
	"time"
)

type checkedKey struct{}

// WithChecked returns ctx marked with whether its box may run the direct exits in its options.
func WithChecked(ctx context.Context, checked bool) context.Context {
	return context.WithValue(ctx, checkedKey{}, checked)
}

// Checked reports whether ctx is marked as allowing its box's direct exits.
func Checked(ctx context.Context) bool {
	checked, _ := ctx.Value(checkedKey{}).(bool)
	return checked
}

type validUntilKey struct{}

// WithValidUntil returns ctx carrying when its box's direct exits must stop: the end of the
// registration that vouched for them.
func WithValidUntil(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, validUntilKey{}, t)
}

// ValidUntil is the deadline WithValidUntil put in ctx, if any. A box without one, such as
// `lantern-box run`'s, which its operator manages, keeps its direct exits.
func ValidUntil(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(validUntilKey{}).(time.Time)
	return t, ok
}
