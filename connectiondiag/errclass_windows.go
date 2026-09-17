//go:build windows

package connectiondiag

import (
	"errors"

	"golang.org/x/sys/windows"
)

func socketErrorClass(err error) string {
	switch {
	case errors.Is(err, windows.WSAECONNRESET):
		return "reset"
	case errors.Is(err, windows.WSAECONNREFUSED):
		return "refused"
	case errors.Is(err, windows.WSAENETUNREACH):
		return "network_unreachable"
	case errors.Is(err, windows.WSAEHOSTUNREACH):
		return "host_unreachable"
	case errors.Is(err, windows.WSAESHUTDOWN):
		return "broken_pipe"
	case errors.Is(err, windows.WSAETIMEDOUT):
		return "timeout"
	}
	return ""
}
