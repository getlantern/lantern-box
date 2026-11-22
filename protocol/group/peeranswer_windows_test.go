package group

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestPeerAnswered(t *testing.T) {
	assert.True(t, peerAnswered(opErr("wsarecv", syscall.WSAECONNRESET)))
	assert.True(t, peerAnswered(opErr("connectex", windows.WSAECONNREFUSED)))
	assert.False(t, peerAnswered(opErr("wsasend", syscall.WSAECONNABORTED)))
	assert.False(t, peerAnswered(nil))
}

func TestFailedLocally(t *testing.T) {
	assert.True(t, failedLocally(opErr("connectex", windows.WSAENETUNREACH)))
	assert.True(t, failedLocally(opErr("wsasend", windows.WSAENETDOWN)))
	assert.True(t, failedLocally(opErr("wsarecv", syscall.WSAECONNABORTED)))
	assert.False(t, failedLocally(opErr("wsarecv", syscall.WSAECONNRESET)))
	assert.False(t, failedLocally(nil))
}
