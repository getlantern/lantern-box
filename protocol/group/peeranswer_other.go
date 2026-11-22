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
