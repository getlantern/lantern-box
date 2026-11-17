package adapter

import "time"

// HelperPathGrace is how long a rooms (helper) consumer may go without a
// connected WebRTC path before its open streams are reset and it reports its
// helper unavailable. It mirrors broflake's default no-path grace, which the
// outbound passes as WebRTCOptions.ConsumerNoPathGrace: one room attempt's
// worst case, ICE gathering (capped at 8s) plus the 10s a room attempt waits
// for its data channel. The grace only delays those two things. New dials
// fail at once whenever no slot is connected (see the outbound's
// checkHelperConnected), so while a slot reconnects new flows fail even
// though Available stays true: Available is a debounce for the UI, so a
// brief drop doesn't flash the helper as unavailable, not a promise that
// the consumer carries traffic.
const HelperPathGrace = 20 * time.Second

// HelperPathStatus is a rooms consumer's view of its WebRTC path to a helper.
// Connected means a WebRTC data channel to a helper is open, not that the
// egress behind it answers.
type HelperPathStatus struct {
	// Connected reports whether any of the consumer's WebRTC slots is
	// connected to a helper now.
	Connected bool
	// Connecting reports whether no slot has connected since the consumer
	// started, less than HelperPathGrace ago: it is still looking for its
	// helper. A consumer still looking after that is unavailable, as one that
	// lost its helper is: the helper is offline or unreachable, and broflake
	// fails its dials.
	Connecting bool
	// NeverConnected reports whether no slot has connected since the
	// consumer started.
	NeverConnected bool
	// Since is when Connected last changed, the first slot connecting or the
	// last one dropping, or, while NeverConnected, when the consumer started.
	// A consumer restarts, and so starts over, whenever a personal proxy
	// selection narrows or widens its tickets.
	Since time.Time
	// Available reports whether the consumer is connected, or has been
	// disconnected, or looking for its helper since starting, for less than
	// HelperPathGrace. It is a UI debounce: while connecting, or reconnecting
	// within the grace, it is true though new dials fail at once.
	Available bool
}

// NewHelperPathStatus returns, as of now, the status of a path that has been
// connected, or not, since since, which is zero if no slot has connected
// since the consumer started at started.
func NewHelperPathStatus(connected bool, since, started, now time.Time) HelperPathStatus {
	never := !connected && since.IsZero()
	if never {
		since = started
	}
	available := connected || since.IsZero() || now.Sub(since) < HelperPathGrace
	return HelperPathStatus{
		Connected:      connected,
		Connecting:     never && available,
		NeverConnected: never,
		Since:          since,
		Available:      available,
	}
}

// HelperPathReporter is implemented by an outbound whose path to a helper
// can go down while the outbound lives: the rooms (helper) Unbounded
// consumer. It is cheap enough to call on every status read. ok is false if
// the outbound is not a rooms consumer or has not started.
type HelperPathReporter interface {
	HelperPath() (status HelperPathStatus, ok bool)
}
