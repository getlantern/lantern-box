package banditprobe

import "time"

// sendState is the kernel's view of what a TCP socket has sent.
type sendState struct {
	// acked is the total number of bytes the peer has acknowledged.
	acked uint64
	// unacked is the number of bytes written but not yet acknowledged,
	// including bytes not yet sent.
	unacked int
	retrans uint32
	rtt     time.Duration
}
