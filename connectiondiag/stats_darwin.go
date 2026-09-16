package connectiondiag

import "golang.org/x/sys/unix"

func readTCPStats(fd uintptr) (*TCPStats, error) {
	v, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	if err != nil {
		return nil, err
	}
	return &TCPStats{Source: "TCP_CONNECTION_INFO", State: uint32(v.State), RTTUS: u64(uint64(v.Srtt) * 1000), RetransmittedPackets: u64(v.Txretransmitpackets), RetransmittedBytes: u64(v.Txretransmitbytes), SentBytes: u64(v.Txbytes), ReceivedBytes: u64(v.Rxbytes)}, nil
}
