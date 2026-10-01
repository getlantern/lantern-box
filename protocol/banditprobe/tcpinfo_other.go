//go:build !linux

package banditprobe

import (
	"errors"
	"net"
)

func readSendState(*net.TCPConn) (sendState, error) {
	return sendState{}, errors.New("tcp send state is only available on linux")
}
