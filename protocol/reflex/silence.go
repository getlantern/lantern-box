package reflex

import (
	"errors"
	"math/rand/v2"
	"net"
	"os"
	"time"
)

// waitForSilence reads up to one byte from conn with a deadline.
//
// Returns:
//   - (nil, nil)  — the deadline elapsed before any client data arrived.
//     The caller should proceed with the Reflex handshake (send ClientHello).
//   - (data, nil) — the client sent bytes before the deadline. The caller
//     should hand the connection (prepending data) off to the masquerade
//     handler. This is the "not a Lantern client" signal.
//   - (nil, err)  — the connection is dead or errored for another reason.
//
// Lantern Reflex clients send no application data before the server speaks.
// Active probes (replaying ClientHellos, etc.) and accidentally-routed TLS
// clients speak immediately, so any byte within the window is a probe signal.
func waitForSilence(conn net.Conn, timeout time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	defer conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	// n == 0: either timeout (silence, good) or a real error (dead conn).
	if err == nil {
		// Shouldn't happen — Read returning (0, nil) is unusual.
		return nil, nil
	}
	if isTimeout(err) {
		return nil, nil
	}
	return nil, err
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// jitteredTimeout returns base ± a uniform random amount up to jitter.
// If jitter is zero or base is zero, base is returned unchanged.
func jitteredTimeout(base, jitter time.Duration) time.Duration {
	if base <= 0 || jitter <= 0 {
		return base
	}
	// Uniform in [-jitter, +jitter].
	offset := time.Duration(rand.Int64N(int64(2*jitter))) - jitter
	result := base + offset
	if result < 0 {
		return base
	}
	return result
}
