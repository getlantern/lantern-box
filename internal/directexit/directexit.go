// Package directexit marks a box's context as built from options whose direct exits peerrules
// checked. It is internal so that only peerrules can set the mark, and an embedder gets it only
// through peerrules.CheckDirectExit; the volunteer service, which peerrules can't import, reads
// it.
package directexit

import "context"

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
