package clientcontext

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sbox "github.com/sagernet/sing-box"

	box "github.com/getlantern/lantern-box"
	lconstant "github.com/getlantern/lantern-box/constant"
	loption "github.com/getlantern/lantern-box/option"
	samizdat "github.com/getlantern/samizdat"
)

func testSamizdatCertKeyPEM(t *testing.T) (certPEM, keyPEM string) {
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

	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return string(certPEMBytes), string(keyPEMBytes)
}

func testShortID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// ptr is a generic helper to get a pointer to a value.
func ptr[T any](v T) *T { return &v }

// TestSamizdatClientContext verifies that CLIENTINFO is correctly sent and received
// through a samizdat connection. This reproduces the issue where the full Lantern app
// fails with "invalid response: HT" when sending CLIENTINFO through samizdat.
func TestSamizdatClientContext(t *testing.T) {
	// Generate samizdat keys
	privKey, pubKey, err := samizdat.GenerateKeyPair()
	require.NoError(t, err)

	privKeyHex := hex.EncodeToString(privKey)
	pubKeyHex := hex.EncodeToString(pubKey)
	shortID := testShortID(t)
	certPEM, keyPEM := testSamizdatCertKeyPEM(t)

	// Find a free port for the samizdat server
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverPort := serverListener.Addr().(*net.TCPAddr).Port
	serverListener.Close()

	// Find a free port for the client HTTP proxy
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	clientPort := clientListener.Addr().(*net.TCPAddr).Port
	clientListener.Close()

	// Start an HTTP test server (the ultimate destination)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))
	defer httpServer.Close()

	ctx := box.BaseContext()
	logger := log.NewNOPFactory().NewLogger("")

	// --- Server side ---
	serverOpts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{
			{
				Type: lconstant.TypeSamizdat,
				Tag:  "samizdat-in",
				Options: &loption.SamizdatInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     ptr(badoption.Addr(netip.MustParseAddr("127.0.0.1"))),
						ListenPort: uint16(serverPort),
					},
					PrivateKey:       privKeyHex,
					ShortIDs:         []string{shortID},
					CertPEM:          certPEM,
					KeyPEM:           keyPEM,
					MasqueradeDomain: "example.com",
				},
			},
		},
		Route: &option.RouteOptions{
			Rules: []option.Rule{
				{
					Type: constant.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						RawDefaultRule: option.RawDefaultRule{
							Inbound: badoption.Listable[string]{"samizdat-in"},
						},
						RuleAction: option.RuleAction{
							Action: constant.RuleActionTypeRoute,
							RouteOptions: option.RouteActionOptions{
								Outbound: "direct",
							},
						},
					},
				},
			},
		},
	}

	serverBox, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: serverOpts,
	})
	require.NoError(t, err)

	// Add Manager tracker on the server
	mgr := NewManager(MatchBounds{[]string{""}, []string{""}}, logger)
	mTracker := &mockTracker{}
	mgr.AppendTracker(mTracker)
	serverBox.Router().AppendTracker(mgr)

	require.NoError(t, serverBox.Start())
	defer serverBox.Close()

	// --- Client side ---
	clientOpts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{
			{
				Type: constant.TypeHTTP,
				Tag:  "http-client",
				Options: &option.HTTPMixedInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     ptr(badoption.Addr(netip.MustParseAddr("127.0.0.1"))),
						ListenPort: uint16(clientPort),
					},
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type: lconstant.TypeSamizdat,
				Tag:  "samizdat-out",
				Options: &loption.SamizdatOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: uint16(serverPort),
					},
					PublicKey:  pubKeyHex,
					ShortID:    shortID,
					ServerName: "127.0.0.1",
				},
			},
		},
		Route: &option.RouteOptions{
			Rules: []option.Rule{
				{
					Type: constant.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						RawDefaultRule: option.RawDefaultRule{
							Inbound: badoption.Listable[string]{"http-client"},
						},
						RuleAction: option.RuleAction{
							Action: constant.RuleActionTypeRoute,
							RouteOptions: option.RouteActionOptions{
								Outbound: "samizdat-out",
							},
						},
					},
				},
			},
		},
	}

	clientBox, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: clientOpts,
	})
	require.NoError(t, err)

	// Add ClientContextInjector on the client
	cInfo := ClientInfo{
		DeviceID:    "test-device",
		Platform:    "test",
		IsPro:       true,
		CountryCode: "IR",
		Version:     "1.0",
	}
	injector := NewClientContextInjector(
		func() ClientInfo { return cInfo },
		MatchBounds{[]string{""}, []string{""}},
	)
	clientBox.Router().AppendTracker(injector)

	require.NoError(t, clientBox.Start())
	defer clientBox.Close()

	// --- Make request through the proxy chain ---
	proxyURL, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(clientPort))
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 10 * time.Second,
	}

	resp, err := httpClient.Get(httpServer.URL)
	require.NoError(t, err, "HTTP request through samizdat proxy should succeed")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the server received the client info
	assert.NotNil(t, mTracker.info, "server should have received CLIENTINFO through samizdat")
	if mTracker.info != nil {
		assert.Equal(t, cInfo.DeviceID, mTracker.info.DeviceID)
		assert.Equal(t, cInfo.Platform, mTracker.info.Platform)
		assert.Equal(t, cInfo.IsPro, mTracker.info.IsPro)
		assert.Equal(t, cInfo.CountryCode, mTracker.info.CountryCode)
		assert.Equal(t, cInfo.Version, mTracker.info.Version)
	}
}
