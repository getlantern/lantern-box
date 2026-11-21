//go:build with_quic

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	sbox "github.com/sagernet/sing-box"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

// TestOutlineQUICPrelude routes a real QUIC handshake through the outline
// outbound's UDP path to a sink that records what reaches the wire. With
// quicprelude the first datagram must be a decoy Initial carrying a reserved
// version, followed by the client's real v1 Initial, both from the same source
// port. With plain UDP the Initial must come first.
func TestOutlineQUICPrelude(t *testing.T) {
	for _, tc := range []struct {
		udp     string
		prelude bool
	}{
		{"quicprelude:count=1", true},
		{"", false},
	} {
		t.Run(fmt.Sprintf("udp=%q", tc.udp), func(t *testing.T) {
			boxCtx := box.BaseContext()
			sink := startUDPSink(t)
			clientPort := freePort(t)
			startBox(t, boxCtx, mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": %d}],
				"outbounds": [{"type": "outline", "tag": "proxyless",
					"domains": ["example.com"], "udp": %q}]
			}`, clientPort, tc.udp)))

			sc := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			pc, err := sc.ListenPacket(ctx, M.SocksaddrFromNet(sink.addr))
			require.NoError(t, err)
			defer pc.Close()
			go func() {
				// The sink never answers, so the handshake only has to start.
				conn, err := quic.DialEarly(ctx, pc, sink.addr,
					&tls.Config{ServerName: "example.com", NextProtos: []string{"h3"}}, &quic.Config{})
				if err == nil {
					conn.CloseWithError(0, "")
				}
			}()

			want := 1
			if tc.prelude {
				want = 2
			}
			got := sink.waitFor(t, want)
			if tc.prelude {
				require.True(t, isReservedVersionLongHeader(got[0].data),
					"first datagram must be the decoy, got % x", got[0].data[:min(8, len(got[0].data))])
				require.True(t, isQUICv1Initial(got[1].data), "the real Initial must follow the decoy")
				require.Equal(t, got[0].from.String(), got[1].from.String(),
					"the decoy and the Initial must share a four-tuple")
			} else {
				require.True(t, isQUICv1Initial(got[0].data), "without a prelude the Initial comes first")
			}
		})
	}
}

// TestOutlineQUICPreludeHandshake completes a QUIC handshake through the
// outline outbound against a real QUIC server, a stock hysteria2 inbound, so
// the server must tolerate the decoy and replies must find their way back.
// It drives the outbound's ListenPacket directly: sing's SOCKS5 UDP server
// races between its read and write paths under -race, and the routed path is
// covered by TestOutlineQUICPrelude.
func TestOutlineQUICPreludeHandshake(t *testing.T) {
	for _, udp := range []string{"quicprelude:count=1", "quicprelude:count=3&version=draft", ""} {
		t.Run(fmt.Sprintf("udp=%q", udp), func(t *testing.T) {
			boxCtx := box.BaseContext()
			serverPort := freeUDPPort(t)
			startBox(t, boxCtx, mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"inbounds": [{"type": "hysteria2", "listen": "127.0.0.1", "listen_port": %d,
					"users": [{"password": "pw"}], "tls": {"enabled": true, "insecure": true}}],
				"outbounds": [{"type": "direct"}]
			}`, serverPort)))
			client, err := sbox.New(sbox.Options{Context: boxCtx, Options: mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"outbounds": [{"type": "outline", "tag": "proxyless", "domains": ["example.com"], "udp": %q}]
			}`, udp))})
			require.NoError(t, err)
			require.NoError(t, client.Start())
			t.Cleanup(func() { client.Close() })
			out, ok := client.Outbound().Outbound("proxyless")
			require.True(t, ok)

			server := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(serverPort)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pc, err := out.ListenPacket(ctx, M.SocksaddrFromNet(server))
			require.NoError(t, err)
			defer pc.Close()
			conn, err := quic.DialEarly(ctx, pc, server,
				&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}, &quic.Config{})
			require.NoError(t, err)
			defer conn.CloseWithError(0, "")
			select {
			case <-conn.HandshakeComplete():
			case <-ctx.Done():
				t.Fatal("QUIC handshake did not complete through the outline outbound")
			}
		})
	}
}

// TestHedgeOutlineQUIC completes a QUIC handshake through a hedge whose First
// is the outline outbound with quicprelude, as radiance builds it, inside a
// running box whose dialers track connections. Then blocks, so the handshake
// can only complete over First. Like TestOutlineQUICPreludeHandshake, it
// drives the outbound's ListenPacket directly: sing's SOCKS5 UDP server races
// between its read and write paths under -race.
func TestHedgeOutlineQUIC(t *testing.T) {
	serverCtx := box.BaseContext()
	serverPort := freeUDPPort(t)
	startBox(t, serverCtx, mustOptions(serverCtx, t, fmt.Sprintf(`{
		"log": {"level": "error"},
		"inbounds": [{"type": "hysteria2", "listen": "127.0.0.1", "listen_port": %d,
			"users": [{"password": "pw"}], "tls": {"enabled": true, "insecure": true}}],
		"outbounds": [{"type": "direct"}]
	}`, serverPort)))
	clientCtx := box.BaseContext()
	client, err := sbox.New(sbox.Options{Context: clientCtx, Options: mustOptions(clientCtx, t, `{
		"log": {"level": "error"},
		"outbounds": [
			{"type": "hedge", "tag": "smart", "first": "proxyless", "then": "blocked", "commit_timeout": "2s"},
			{"type": "outline", "tag": "proxyless", "domains": ["example.com"], "udp": "quicprelude:count=1"},
			{"type": "block", "tag": "blocked"}
		]
	}`)})
	require.NoError(t, err)
	require.NoError(t, client.Start())
	t.Cleanup(func() { client.Close() })
	out, ok := client.Outbound().Outbound("smart")
	require.True(t, ok)

	server := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(serverPort)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := out.ListenPacket(ctx, M.SocksaddrFromNet(server))
	require.NoError(t, err)
	defer pc.Close()
	conn, err := quic.DialEarly(ctx, pc, server,
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}, &quic.Config{})
	require.NoError(t, err)
	defer conn.CloseWithError(0, "")
	select {
	case <-conn.HandshakeComplete():
	case <-ctx.Done():
		t.Fatal("QUIC handshake did not complete through the hedge's outline First")
	}
}

// TestOutlineUDPToDomain sends UDP to a domain-name destination through the
// outline outbound, as a SOCKS5 or HTTP inbound does: the outbound must
// resolve it rather than write to an empty address.
func TestOutlineUDPToDomain(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(buf[:n], from)
		}
	}()
	boxCtx := box.BaseContext()
	client, err := sbox.New(sbox.Options{Context: boxCtx, Options: mustOptions(boxCtx, t, `{
		"log": {"level": "error"},
		"dns": {"servers": [{"type": "local", "tag": "local"}]},
		"outbounds": [{"type": "outline", "tag": "proxyless", "domains": ["example.com"], "udp": "",
			"domain_resolver": {"server": "local", "strategy": "ipv4_only"}}]
	}`)})
	require.NoError(t, err)
	require.NoError(t, client.Start())
	t.Cleanup(func() { client.Close() })
	out, ok := client.Outbound().Outbound("proxyless")
	require.True(t, ok)

	dest := M.ParseSocksaddrHostPort("localhost", uint16(echo.LocalAddr().(*net.UDPAddr).Port))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := out.DialContext(ctx, "udp", dest)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf[:n]))
}

// isReservedVersionLongHeader reports whether b is a long-header packet whose
// version is a reserved 0x?a?a?a?a codepoint (RFC 9000 §15).
func isReservedVersionLongHeader(b []byte) bool {
	return len(b) >= 5 && b[0]&0x80 != 0 &&
		binary.BigEndian.Uint32(b[1:5])&0x0f0f0f0f == 0x0a0a0a0a
}

type sinkDatagram struct {
	from net.Addr
	data []byte
}

type udpSink struct {
	addr *net.UDPAddr
	mu   sync.Mutex
	got  []sinkDatagram
}

func startUDPSink(t *testing.T) *udpSink {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	s := &udpSink{addr: conn.LocalAddr().(*net.UDPAddr)}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.got = append(s.got, sinkDatagram{from, append([]byte(nil), buf[:n]...)})
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *udpSink) waitFor(t *testing.T, n int) []sinkDatagram {
	t.Helper()
	var got []sinkDatagram
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		got = append([]sinkDatagram(nil), s.got...)
		return len(got) >= n
	}, 3*time.Second, 10*time.Millisecond, "sink received fewer than %d datagrams", n)
	return got
}
