//go:build !windows

package connectiondiag

import (
	"net"
	"os"
	"syscall"
	"testing"
)

func TestNativeSocketErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{{syscall.ECONNRESET, "reset"}, {syscall.ECONNREFUSED, "refused"}, {syscall.ENETUNREACH, "network_unreachable"}, {syscall.EHOSTUNREACH, "host_unreachable"}, {syscall.EPIPE, "broken_pipe"}, {syscall.ETIMEDOUT, "timeout"}} {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: tc.err}}
		if got := errorClass(err); got != tc.want {
			t.Errorf("%v: got %q, want %q", err, got, tc.want)
		}
	}
}
