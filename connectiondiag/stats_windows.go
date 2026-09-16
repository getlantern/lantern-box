package connectiondiag

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type tcpInfoV0 struct {
	State             uint32
	MSS               uint32
	ConnectionTimeMS  uint64
	TimestampsEnabled uint8
	_                 [3]byte
	RTTUS             uint32
	MinRTTUS          uint32
	BytesInFlight     uint32
	Cwnd              uint32
	SndWnd            uint32
	RcvWnd            uint32
	RcvBuf            uint32
	BytesOut          uint64
	BytesIn           uint64
	BytesReordered    uint32
	BytesRetrans      uint32
	FastRetrans       uint32
	DupAcksIn         uint32
	TimeoutEpisodes   uint32
	SynRetrans        uint8
	_                 [3]byte
}

func readTCPStats(fd uintptr) (*TCPStats, error) {
	const sioTCPInfo = 0xD8000027
	var v tcpInfoV0
	// The input version must remain zero while the output uses the TCP_INFO_v0 ABI.
	var version, received uint32
	err := windows.WSAIoctl(windows.Handle(fd), sioTCPInfo, (*byte)(unsafe.Pointer(&version)), 4, (*byte)(unsafe.Pointer(&v)), uint32(unsafe.Sizeof(v)), &received, nil, 0)
	if err != nil {
		return nil, err
	}
	if received < uint32(unsafe.Sizeof(v)) {
		return nil, fmt.Errorf("short TCP_INFO response")
	}
	return &TCPStats{Source: "SIO_TCP_INFO_v0", State: v.State, RTTUS: u64(uint64(v.RTTUS)), RetransmittedBytes: u64(uint64(v.BytesRetrans)), SentBytes: u64(v.BytesOut), ReceivedBytes: u64(v.BytesIn)}, nil
}
