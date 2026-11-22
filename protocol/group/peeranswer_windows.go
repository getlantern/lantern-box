package group

import (
	"errors"
	"syscall"
)

// Winsock errors package syscall doesn't define.
const (
	wsaenetdown     syscall.Errno = 10050
	wsaenetunreach  syscall.Errno = 10051
	wsaeconnrefused syscall.Errno = 10061
)

// peerAnswered reports whether err shows the far end answered: a reset or a
// refusal.
func peerAnswered(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, wsaeconnrefused)
}

// failedLocally reports whether err shows the device failed the conn itself:
// it has no route or no network, or it aborted an established conn.
func failedLocally(err error) bool {
	return errors.Is(err, wsaenetunreach) || errors.Is(err, wsaenetdown) || errors.Is(err, syscall.WSAECONNABORTED)
}
