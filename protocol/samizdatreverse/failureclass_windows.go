//go:build windows

package samizdatreverse

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Sockets on windows fail with Winsock codes, not the errnos syscall
// invents for the POSIX names, so both are listed.
var (
	refusedErrnos      = []syscall.Errno{syscall.ECONNREFUSED, windows.WSAECONNREFUSED}
	unreachableErrnos  = []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH}
	resetErrnos        = []syscall.Errno{syscall.ECONNRESET, syscall.EPIPE, windows.WSAECONNRESET, windows.WSAECONNABORTED, windows.ERROR_BROKEN_PIPE}
	notPermittedErrnos = []syscall.Errno{syscall.EPERM, syscall.EACCES, windows.WSAEACCES}
)
