package banditprobe

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

func readSendState(tc *net.TCPConn) (sendState, error) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return sendState{}, err
	}
	var (
		st     sendState
		optErr error
	)
	err = rc.Control(func(fd uintptr) {
		st, optErr = sendStateFrom(
			func() (int, error) { return unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) },
			func() (*unix.TCPInfo, error) { return unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) },
		)
	})
	if err != nil {
		return sendState{}, err
	}
	return st, optErr
}

// sendStateFrom reads the unacknowledged byte count before the acknowledged
// one. An ack landing between the two reads then raises acked without lowering
// unacked, so acked+unacked can overstate the bytes written so far but never
// understate them; a delivery watermark taken from it is conservative.
func sendStateFrom(outq func() (int, error), info func() (*unix.TCPInfo, error)) (sendState, error) {
	unacked, err := outq()
	if err != nil {
		return sendState{}, err
	}
	ti, err := info()
	if err != nil {
		return sendState{}, err
	}
	return sendState{
		acked:   ti.Bytes_acked,
		unacked: unacked,
		retrans: ti.Total_retrans,
		rtt:     time.Duration(ti.Rtt) * time.Microsecond,
	}, nil
}
