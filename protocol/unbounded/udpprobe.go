package unbounded

import (
	"bytes"
	"crypto/subtle"
	"net"
	"sync/atomic"
)

// udpProbeMagic starts lantern-cloud's UDP probe (peerUDPProbeMagic in lantern-cloud's
// cmd/api/peer_udp_probe.go), ahead of the node's token. No WebRTC packet starts with 0xFF: STUN
// starts below 4, DTLS from 20 to 63 and RTP from 128 to 191.
var udpProbeMagic = []byte{0xFF, 'L', 'N', 'T', 'N', 'P', 'R', '1'}

// UDPProbeTokenLen is the length of the token a node sends lantern-cloud with Verify.
const UDPProbeTokenLen = 16

var udpProbeToken atomic.Pointer[[]byte]

// SetUDPProbeToken sets the token a volunteer with a mapped UDP port echoes lantern-cloud's probe
// for, so a node can show lantern-cloud the mapping reaches it from outside. Only probes carrying
// it are echoed, so nobody without it can bounce traffic off the node. Nil echoes nothing.
func SetUDPProbeToken(token []byte) {
	if len(token) == 0 {
		udpProbeToken.Store(nil)
		return
	}
	t := bytes.Clone(token)
	udpProbeToken.Store(&t)
}

// probeEchoConn answers lantern-cloud's probes on a volunteer's mapped UDP socket and hands
// everything else to the ICE mux. It embeds the interface, not the *net.UDPConn, so pion's mux
// reads through ReadFrom rather than around it.
type probeEchoConn struct {
	net.PacketConn
}

func (c probeEchoConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || n < len(udpProbeMagic) || !bytes.Equal(p[:len(udpProbeMagic)], udpProbeMagic) {
			return n, addr, err
		}
		// A probe is never WebRTC traffic, so one without the current token is dropped rather
		// than passed on.
		if token := udpProbeToken.Load(); token != nil && n == len(udpProbeMagic)+UDPProbeTokenLen &&
			subtle.ConstantTimeCompare(p[len(udpProbeMagic):n], *token) == 1 {
			_, _ = c.PacketConn.WriteTo(p[:n], addr)
		}
	}
}
