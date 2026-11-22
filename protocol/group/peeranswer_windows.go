package group

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is Winsock's WSAECONNREFUSED, which package syscall doesn't
// define.
const wsaeconnrefused syscall.Errno = 10061

// peerAnswered reports whether err shows the far end answered: a reset or a
// refusal.
func peerAnswered(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, wsaeconnrefused)
}
