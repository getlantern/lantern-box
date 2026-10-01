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
		var info *unix.TCPInfo
		info, optErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if optErr != nil {
			return
		}
		st.acked = info.Bytes_acked
		st.retrans = info.Total_retrans
		st.rtt = time.Duration(info.Rtt) * time.Microsecond
		st.unacked, optErr = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ)
	})
	if err != nil {
		return sendState{}, err
	}
	return st, optErr
}
