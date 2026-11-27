//go:build !windows

package group

import (
	"errors"
	"syscall"
)

// peerAnswered reports whether err shows the far end answered: a reset or a
// refusal. A device that has lost its own network fails locally instead, as
// Android does with ECONNABORTED or ENETUNREACH, or times out.
func peerAnswered(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

// failedLocally reports whether err shows the device failed the conn itself:
// it has no route or no network, as a device that lost its network reports
// for new conns, or it aborted an established one, as Android does then.
// EHOSTUNREACH isn't included, since a router on the way can report it.
func failedLocally(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ENETDOWN) || errors.Is(err, syscall.ECONNABORTED)
}
