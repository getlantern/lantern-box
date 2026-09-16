//go:build !linux && !darwin && !windows

package connectiondiag

import "errors"

func readTCPStats(fd uintptr) (*TCPStats, error) {
	return nil, errors.New("TCP socket statistics unavailable")
}
