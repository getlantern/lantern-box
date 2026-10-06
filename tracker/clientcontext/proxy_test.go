package clientcontext

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lAdapter "github.com/getlantern/lantern-box/adapter"
	lconstant "github.com/getlantern/lantern-box/constant"
	loption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/samizdat"
)

func serverOptions(port uint16) option.ServerOptions {
	return option.ServerOptions{Server: "127.0.0.1", ServerPort: port}
}

func startHTTPPair(t *testing.T, patch ...func(*option.Options)) (*mockTracker, *testClient) {
	return startProxyPair(t,
		func(port uint16) option.Inbound {
			return option.Inbound{Type: constant.TypeHTTP, Tag: "http-in",
				Options: &option.HTTPMixedInboundOptions{ListenOptions: loopbackListen(port)}}
		},
		func(port uint16) option.Outbound {
			return option.Outbound{Type: constant.TypeHTTP, Tag: "http-out",
				Options: &option.HTTPOutboundOptions{ServerOptions: serverOptions(port)}}
		},
		patch...,
	)
}

func startShadowsocksPair(t *testing.T, patch ...func(*option.Options)) (*mockTracker, *testClient) {
	const method, password = "aes-128-gcm", "test-password"
	return startProxyPair(t,
		func(port uint16) option.Inbound {
			return option.Inbound{Type: constant.TypeShadowsocks, Tag: "ss-in",
				Options: &option.ShadowsocksInboundOptions{ListenOptions: loopbackListen(port), Method: method, Password: password}}
		},
		func(port uint16) option.Outbound {
			return option.Outbound{Type: constant.TypeShadowsocks, Tag: "ss-out",
				Options: &option.ShadowsocksOutboundOptions{ServerOptions: serverOptions(port), Method: method, Password: password}}
		},
		patch...,
	)
}

func startVMessPair(t *testing.T, patch ...func(*option.Options)) (*mockTracker, *testClient) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	return startProxyPair(t,
		func(port uint16) option.Inbound {
			return option.Inbound{Type: constant.TypeVMess, Tag: "vmess-in",
				Options: &option.VMessInboundOptions{ListenOptions: loopbackListen(port), Users: []option.VMessUser{{Name: "test", UUID: uuid}}}}
		},
		func(port uint16) option.Outbound {
			return option.Outbound{Type: constant.TypeVMess, Tag: "vmess-out",
				Options: &option.VMessOutboundOptions{ServerOptions: serverOptions(port), UUID: uuid, Security: "aes-128-gcm"}}
		},
		patch...,
	)
}

func startSamizdatPair(t *testing.T, patch ...func(*option.Options)) (*mockTracker, *testClient) {
	privKey, pubKey, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)
	shortID := make([]byte, 8)
	_, err = rand.Read(shortID)
	require.NoError(t, err)
	certPEM, keyPEM := selfSignedCert(t)
	return startProxyPair(t,
		func(port uint16) option.Inbound {
			return option.Inbound{Type: lconstant.TypeSamizdat, Tag: "samizdat-in",
				Options: &loption.SamizdatInboundOptions{
					ListenOptions:    loopbackListen(port),
					PrivateKey:       hex.EncodeToString(privKey),
					ShortIDs:         []string{hex.EncodeToString(shortID)},
					CertPEM:          certPEM,
					KeyPEM:           keyPEM,
					MasqueradeDomain: "example.com",
				}}
		},
		func(port uint16) option.Outbound {
			return option.Outbound{Type: lconstant.TypeSamizdat, Tag: "samizdat-out",
				Options: &loption.SamizdatOutboundOptions{
					ServerOptions: serverOptions(port),
					PublicKey:     hex.EncodeToString(pubKey),
					ShortID:       hex.EncodeToString(shortID),
					ServerName:    "127.0.0.1",
				}}
		},
		patch...,
	)
}

func selfSignedCert(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// echoPacket sends payload to destination over conn and returns the reply.
func echoPacket(t *testing.T, conn net.PacketConn, destination M.Socksaddr, payload string) string {
	t.Helper()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err := conn.WriteTo([]byte(payload), destination.UDPAddr())
	require.NoError(t, err)
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFrom(buf)
	require.NoError(t, err)
	return string(buf[:n])
}

// Every dial through an enabled outbound carries client info, and the traffic
// behind the frame arrives intact.
func TestProxy(t *testing.T) {
	tests := []struct {
		name  string
		start func(*testing.T, ...func(*option.Options)) (*mockTracker, *testClient)
		tag   string
		// frameConn reports whether the outbound's conns postpone their
		// handshake through N.EarlyConn and so are wrapped in a frameConn.
		// vmess postpones through N.EarlyWriter only.
		frameConn bool
		udp       bool
	}{
		{name: "http", start: startHTTPPair, tag: "http-out"},
		{name: "shadowsocks", start: startShadowsocksPair, tag: "ss-out", frameConn: true, udp: true},
		{name: "vmess", start: startVMessPair, tag: "vmess-out"},
		{name: "samizdat", start: startSamizdatPair, tag: "samizdat-out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker, client := tt.start(t)

			t.Run("router", func(t *testing.T) {
				tracker.reset()
				client.get(t)
				assert.Equal(t, &testClientInfo, tracker.info())
			})

			t.Run("direct", func(t *testing.T) {
				tracker.reset()
				conn := client.dial(t, context.Background(), N.NetworkTCP, tt.tag)
				_, wrapped := conn.(*frameConn)
				require.Equal(t, tt.frameConn, wrapped)
				if wrapped {
					early, ok := common.Cast[N.EarlyConn](conn)
					require.True(t, ok)
					assert.True(t, early.NeedHandshake(), "probes must still see the postponed handshake")
				}
				client.getOver(t, conn)
				assert.Equal(t, &testClientInfo, tracker.info())
			})

			t.Run("probe", func(t *testing.T) {
				tracker.reset()
				client.getOver(t, client.dial(t, lAdapter.ContextWithProbe(context.Background()), N.NetworkTCP, tt.tag))
				assert.Nil(t, tracker.info(), "the server must not attribute a probe to a client")
			})

			if !tt.udp {
				return
			}
			out, ok := client.box.Outbound().Outbound(tt.tag)
			require.True(t, ok)

			// The frame goes out as its own datagram, and the datagrams after it
			// pass through intact.
			t.Run("listen packet", func(t *testing.T) {
				tracker.reset()
				echo := startUDPEcho(t)
				conn, err := out.ListenPacket(context.Background(), echo)
				require.NoError(t, err)
				defer conn.Close()

				assert.Equal(t, "ping", echoPacket(t, conn, echo, "ping"))
				assert.Equal(t, "pong", echoPacket(t, conn, echo, "pong"))
				assert.Equal(t, &testClientInfo, tracker.info())
			})

			t.Run("dial udp", func(t *testing.T) {
				tracker.reset()
				echo := startUDPEcho(t)
				conn, err := out.DialContext(context.Background(), N.NetworkUDP, echo)
				require.NoError(t, err)
				defer conn.Close()
				require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

				_, err = conn.Write([]byte("ping"))
				require.NoError(t, err)
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				require.NoError(t, err)
				assert.Equal(t, "ping", string(buf[:n]))
				assert.Equal(t, &testClientInfo, tracker.info())
			})
		})
	}
}

// startGreetingServer starts a TCP server that sends greeting on each
// connection before reading anything, like SMTP or SSH.
func startGreetingServer(t *testing.T, greeting string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte(greeting))
			go func() {
				io.Copy(io.Discard, conn)
				conn.Close()
			}()
		}
	}()
	return l.Addr().String()
}

// A server-first flow sends nothing until the destination greets it, so the
// frame must go out with the router's handshake kick, including through
// lantern-box's groups, which wrap member conns.
func TestServerFirst(t *testing.T) {
	// mutableurltest returns member conns wrapped in an adapter.TaggedConn. Its
	// URL tests go to a local server so the test stays offline.
	urlTest := startHTTPServer(t)
	inGroup := func(opts *option.Options) {
		opts.Outbounds = append(opts.Outbounds, option.Outbound{Type: lconstant.TypeMutableURLTest, Tag: "group",
			Options: &loption.MutableURLTestOutboundOptions{
				Outbounds:   []string{"ss-out"},
				URL:         urlTest.URL,
				Interval:    badoption.Duration(time.Hour),
				IdleTimeout: badoption.Duration(time.Hour),
			}})
		opts.Route = &option.RouteOptions{Final: "group"}
	}
	tests := []struct {
		name  string
		patch []func(*option.Options)
	}{
		{name: "outbound"},
		{name: "group", patch: []func(*option.Options){inGroup}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker, client := startShadowsocksPair(t, tt.patch...)
			const greeting = "220 ready\r\n"
			dest := startGreetingServer(t, greeting)

			conn, err := net.Dial("tcp", client.proxyAddr)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dest, dest)
			require.NoError(t, err)
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			got, err := br.ReadString('\n')
			require.NoError(t, err, "the greeting must arrive without the client writing first")
			assert.Equal(t, greeting, got)
			assert.Equal(t, &testClientInfo, tracker.info())
		})
	}
}
