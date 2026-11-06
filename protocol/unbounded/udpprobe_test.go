package unbounded

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	sblog "github.com/sagernet/sing-box/log"
	sbOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

func testUDPProbe(token []byte) []byte {
	return append(append([]byte{}, udpProbeMagic...), token...)
}

func setTestUDPProbeToken(t *testing.T, token []byte) {
	t.Helper()
	SetUDPProbeToken(token)
	t.Cleanup(func() { SetUDPProbeToken(nil) })
}

// readUDP returns the next datagram conn receives within wait, or nil.
func readUDP(t *testing.T, conn *net.UDPConn, wait time.Duration) []byte {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(wait)))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

// The wrapper echoes a probe carrying the current token, drops any other probe, and hands
// everything else to whoever reads it, as the ICE mux does.
func TestProbeEchoConn(t *testing.T) {
	token := bytes.Repeat([]byte{4}, UDPProbeTokenLen)
	setTestUDPProbeToken(t, token)

	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	read := make(chan []byte, 8)
	go func() {
		conn := probeEchoConn{server}
		buf := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			read <- bytes.Clone(buf[:n])
		}
	}()

	client, err := net.DialUDP("udp4", nil, server.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	_, err = client.Write(testUDPProbe(token))
	require.NoError(t, err)
	assert.Equal(t, testUDPProbe(token), readUDP(t, client, 2*time.Second), "a probe with the token is echoed")

	for name, probe := range map[string][]byte{
		"wrong token":     testUDPProbe(bytes.Repeat([]byte{5}, UDPProbeTokenLen)),
		"truncated token": testUDPProbe(token[:8]),
	} {
		_, err = client.Write(probe)
		require.NoError(t, err)
		assert.Nil(t, readUDP(t, client, 200*time.Millisecond), "%s: not echoed", name)
	}

	stun := []byte{0x00, 0x01, 0x00, 0x00}
	_, err = client.Write(stun)
	require.NoError(t, err)
	select {
	case got := <-read:
		assert.Equal(t, stun, got, "only ordinary traffic reaches the reader")
	case <-time.After(2 * time.Second):
		t.Fatal("the STUN packet never reached the reader")
	}

	SetUDPProbeToken(nil)
	_, err = client.Write(testUDPProbe(token))
	require.NoError(t, err)
	assert.Nil(t, readUDP(t, client, 200*time.Millisecond), "without a token nothing is echoed")
	select {
	case got := <-read:
		t.Fatalf("a probe reached the reader: %v", got)
	default:
	}
}

// An external address is the mapped mode: the volunteer offers it to consumers and reports itself
// mapped to freddie. Without one the port is used, but nothing is claimed.
func TestNewVolunteer_MappedPort(t *testing.T) {
	opts := validVolunteerOptions()
	opts.MappedUDPPort = 30123
	v := newTestVolunteer(t, opts)
	assert.EqualValues(t, 30123, v.mappedPort)
	assert.Empty(t, v.rtcOpt.ICEUDPMuxExternalIP)
	assert.False(t, v.rtcOpt.VolunteerMapped)

	opts.MappedExternalIP = "198.51.100.7"
	v = newTestVolunteer(t, opts)
	assert.Equal(t, "198.51.100.7", v.rtcOpt.ICEUDPMuxExternalIP)
	assert.True(t, v.rtcOpt.VolunteerMapped)
}

func TestNewVolunteer_MappedPortRequiresAPortAndIPv4(t *testing.T) {
	for name, mutate := range map[string]func(*option.UnboundedVolunteerOptions){
		"no port": func(o *option.UnboundedVolunteerOptions) { o.MappedExternalIP = "198.51.100.7" },
		"ipv6":    func(o *option.UnboundedVolunteerOptions) { o.MappedUDPPort, o.MappedExternalIP = 30123, "2001:db8::1" },
		"garbage": func(o *option.UnboundedVolunteerOptions) { o.MappedUDPPort, o.MappedExternalIP = 30123, "nope" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := validVolunteerOptions()
			mutate(&opts)
			_, err := NewVolunteer(context.Background(), sblog.NewNOPFactory().Logger(), "volunteer", opts)
			assert.ErrorContains(t, err, "mapped_external_ip")
		})
	}
}

// A box running the volunteer on a mapped port answers lantern-cloud's probe there, and frees the
// port when it closes, so the node's next box can take it.
func TestVolunteerAnswersProbesOnItsMappedPort(t *testing.T) {
	token := bytes.Repeat([]byte{6}, UDPProbeTokenLen)
	setTestUDPProbeToken(t, token)
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)

	probeConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	port := probeConn.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, probeConn.Close())

	for _, limit := range []string{"", "10mbit"} {
		t.Run("bandwidth "+limit, func(t *testing.T) {
			ctx := newVolunteerBoxContext(t)
			opts, err := json.UnmarshalExtendedContext[sbOption.Options](ctx, []byte(fmt.Sprintf(`{
				"log": {"level": "info", "output": %q},
				"services": [{
					"type": "unbounded",
					"tag": "volunteer",
					"discovery_srv": %q,
					"egress_addr": "ws://127.0.0.1:1",
					"stun_servers": ["stun:127.0.0.1:9"],
					"bandwidth_limit": %q,
					"mapped_udp_port": %d,
					"mapped_external_ip": "127.0.0.1",
					"stats_interval": -1
				}]
			}`, filepath.Join(t.TempDir(), "box.log"), unreachable.URL, limit, port)))
			require.NoError(t, err)
			b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
			require.NoError(t, err)
			require.NoError(t, b.Start())

			client, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
			require.NoError(t, err)
			defer func() { _ = client.Close() }()
			var echoed []byte
			for range 10 {
				_, err = client.Write(testUDPProbe(token))
				require.NoError(t, err)
				if echoed = readUDP(t, client, 200*time.Millisecond); echoed != nil {
					break
				}
			}
			assert.Equal(t, testUDPProbe(token), echoed, "the running volunteer echoes the probe")

			closed := make(chan error, 1)
			go func() { closed <- b.Close() }()
			select {
			case err := <-closed:
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("closing the box hung")
			}
			again, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
			require.NoError(t, err, "the port must be free once the box closes")
			require.NoError(t, again.Close())
		})
	}
}
