package group

import (
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPeerAnswered(t *testing.T) {
	opErr := func(op string, errno syscall.Errno) error {
		return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, errno)}
	}
	assert.True(t, peerAnswered(opErr("wsarecv", syscall.WSAECONNRESET)))
	assert.True(t, peerAnswered(opErr("connectex", wsaeconnrefused)))
	assert.False(t, peerAnswered(opErr("wsasend", syscall.WSAECONNABORTED)))
	assert.False(t, peerAnswered(nil))
}

func TestFailedLocally(t *testing.T) {
	opErr := func(op string, errno syscall.Errno) error {
		return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, errno)}
	}
	assert.True(t, failedLocally(opErr("connectex", wsaenetunreach)))
	assert.True(t, failedLocally(opErr("wsasend", wsaenetdown)))
	assert.True(t, failedLocally(opErr("wsarecv", syscall.WSAECONNABORTED)))
	assert.False(t, failedLocally(opErr("wsarecv", syscall.WSAECONNRESET)))
	assert.False(t, failedLocally(nil))
}
