package group

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// peerAnswered reports whether err shows the far end answered: a reset or a
// refusal.
func peerAnswered(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, windows.WSAECONNREFUSED)
}

// failedLocally reports whether err shows the device failed the conn itself:
// it has no route or no network, or it aborted an established conn.
func failedLocally(err error) bool {
	return errors.Is(err, windows.WSAENETUNREACH) || errors.Is(err, windows.WSAENETDOWN) || errors.Is(err, syscall.WSAECONNABORTED)
}
