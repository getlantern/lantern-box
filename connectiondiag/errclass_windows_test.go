//go:build windows

package connectiondiag

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativeSocketErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{{windows.WSAECONNRESET, "reset"}, {windows.WSAECONNREFUSED, "refused"}, {windows.WSAENETUNREACH, "network_unreachable"}, {windows.WSAEHOSTUNREACH, "host_unreachable"}, {windows.WSAESHUTDOWN, "broken_pipe"}, {windows.WSAETIMEDOUT, "timeout"}} {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: tc.err}}
		if got := errorClass(err); got != tc.want {
			t.Errorf("%v: got %q, want %q", err, got, tc.want)
		}
	}
}
