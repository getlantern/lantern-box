package adapter

import "time"

// HelperPathGrace is how long a rooms (helper) consumer may go without a
// connected WebRTC path before it reports its helper unavailable. It is also
// the grace after which broflake fails the consumer's QUIC streams and dials
// (WebRTCOptions.ConsumerNoPathGrace), so the two agree: a slot that drops
// and reconnects at once, as a data channel sometimes does, never shows, and
// a path that stays down is reported down when its flows start failing.
const HelperPathGrace = 10 * time.Second

// HelperPathStatus is a rooms consumer's view of its WebRTC path to a helper.
// Connected means a WebRTC data channel to a helper is open, not that the
// egress behind it answers.
type HelperPathStatus struct {
	// Connected reports whether any of the consumer's WebRTC slots is
	// connected to a helper now.
	Connected bool
	// Connecting reports whether no slot has connected since the consumer
	// started: it is still looking for its helper, and has lost nothing.
	Connecting bool
	// Since is when Connected last changed, the first slot connecting or the
	// last one dropping, or zero while Connecting.
	Since time.Time
	// Available is false once the consumer has lost its helper: a slot was
	// connected, and none has been for HelperPathGrace. It is true while
	// Connecting, which is not a loss but carries no traffic either.
	Available bool
}

// NewHelperPathStatus returns, as of now, the status of a path that has been
// connected, or not, since since, zero if it never has been.
func NewHelperPathStatus(connected bool, since, now time.Time) HelperPathStatus {
	connecting := !connected && since.IsZero()
	return HelperPathStatus{
		Connected:  connected,
		Connecting: connecting,
		Since:      since,
		Available:  connected || connecting || now.Sub(since) < HelperPathGrace,
	}
}

// HelperPathReporter is implemented by an outbound whose path to a helper
// can go down while the outbound lives: the rooms (helper) Unbounded
// consumer. It is cheap enough to call on every status read. ok is false if
// the outbound is not a rooms consumer or has not started.
type HelperPathReporter interface {
	HelperPath() (status HelperPathStatus, ok bool)
}
