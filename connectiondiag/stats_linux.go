package connectiondiag

import "golang.org/x/sys/unix"

func readTCPStats(fd uintptr) (*TCPStats, error) {
	v, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return nil, err
	}
	return &TCPStats{Source: "TCP_INFO", State: uint32(v.State), RTTUS: u64(uint64(v.Rtt)), RetransmittedPackets: u64(uint64(v.Total_retrans))}, nil
}
