package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"

	box "github.com/getlantern/lantern-box"
	lbOption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol"
)

// TestMITMDFE2E exercises the MITM-DomainFronting outbound end-to-end:
//
//   - lantern-box generates a name-constrained CA on first boot
//     (permitted: vercel.app, vercel.com).
//   - A SOCKS5 mixed inbound routes "evil.test" -> wait, evil.test isn't
//     covered. We use "evil.vercel.app" so the SNI falls inside the CA's
//     PermittedDNSDomains and the fronts entry's Names ["vercel.app"]
//     suffix-matches it.
//   - The fronts entry says front "evil.vercel.app" onto fronted_sni
//     "front.test" via redirect_addr to an in-process stub fronting
//     server using a private fronting CA pinned via verify_san.
//   - Wait — verify_san only loosens *which name* the cert is valid for,
//     it doesn't change the trust anchor. The system root pool needs the
//     fronting CA. Since this is a test, we ship the fronting cert as
//     verify_san target and ALSO need the fronting CA in the system
//     trust. Instead we use accepted_cert_names via verify_san and a
//     fronting CA that signs a cert whose system-root chain works.
//
// Practical approach: since we control the fronting CA only in-test, we
// substitute the fronts-side certificate with one whose chain we add to
// the OS root pool via SSL_CERT_FILE env var for this process. That
// matches how real Lantern operators would rely on system roots for CDN
// edges (Let's Encrypt, DigiCert, etc.).
//
// Assertions:
//   - User-side leaf is signed by the lantern CA and is valid for
//     evil.vercel.app.
//   - Egress saw SNI=front.test and ALPN=[h2] (inheritance fix proven).
//   - JSONL audit log received an `allow` record for evil.vercel.app.
//   - Bytes round-trip.
func TestMITMDFE2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dataDir := t.TempDir()
	caCertPath := filepath.Join(dataDir, "mitm-ca.crt")
	caKeyPath := filepath.Join(dataDir, "mitm-ca.key")
	auditPath := filepath.Join(dataDir, "mitm-audit.log")

	// Fronting-side CA + leaf, used by the stub front server. To make
	// uTLS's system-roots chain check happy for the egress handshake, we
	// stage the fronting CA into a file and point SSL_CERT_FILE at it.
	frontCACert, frontCAKey := genTestCA(t, "test-front-CA")
	frontLeaf := issueTestLeaf(t, frontCACert, frontCAKey, "front.test")
	frontCAPath := filepath.Join(dataDir, "front-ca.pem")
	require.NoError(t, os.WriteFile(frontCAPath, pemCert(frontCACert.Raw), 0o644))
	t.Setenv("SSL_CERT_FILE", frontCAPath)

	// Stub fronting server.
	captured := newCaptureChan()
	frontListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = frontListener.Close() })

	frontTLSConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			captured.send(captureEntry{
				ServerName: chi.ServerName,
				ALPN:       append([]string(nil), chi.SupportedProtos...),
			})
			return &frontLeaf, nil
		},
	}
	go runFrontServer(ctx, frontListener, frontTLSConfig)

	boxCtx := box.BaseContext()
	boxCtx = protocol.RegisterProtocols(boxCtx)

	inboundPort := freePort(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	mitmOpts := lbOption.MITMDFOutboundOptions{
		CA: lbOption.MITMDFCAOptions{
			CertPath:         caCertPath,
			KeyPath:          caKeyPath,
			PermittedDomains: []string{"vercel.app", "vercel.com"},
			Validity:         "24h",
		},
		Fronts: []lbOption.MITMDFFrontEntry{
			{
				Names:        []string{"vercel.app"},
				FrontedSNI:   "front.test",
				RedirectAddr: frontListener.Addr().String(),
			},
		},
		AuditLogPath: auditPath,
		Fingerprint:  "chrome",
	}

	opts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{{
			Type: "mixed",
			Tag:  "mixed-in",
			Options: &option.HTTPMixedInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     &listenAddr,
					ListenPort: inboundPort,
				},
			},
		}},
		Outbounds: []option.Outbound{{
			Type:    "mitm-df",
			Tag:     "mitm-df-out",
			Options: &mitmOpts,
		}},
		Route: &option.RouteOptions{Final: "mitm-df-out"},
	}

	b, err := sbox.New(sbox.Options{Context: boxCtx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, b.Start())
	t.Cleanup(func() { _ = b.Close() })

	// SOCKS5-connect to evil.vercel.app:443 and run TLS on the tunneled
	// conn trusting only the just-generated lantern CA.
	socksDialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", inboundPort), nil, proxy.Direct)
	require.NoError(t, err)

	tunnelCtx, tunnelCancel := context.WithTimeout(ctx, 10*time.Second)
	defer tunnelCancel()
	rawConn, err := socksDialer.(proxy.ContextDialer).DialContext(tunnelCtx, "tcp", "evil.vercel.app:443")
	require.NoError(t, err)
	defer rawConn.Close()

	caPEM, err := os.ReadFile(caCertPath)
	require.NoError(t, err)
	lanternCAPool := x509.NewCertPool()
	require.True(t, lanternCAPool.AppendCertsFromPEM(caPEM))

	userTLS := tls.Client(rawConn, &tls.Config{
		ServerName: "evil.vercel.app",
		RootCAs:    lanternCAPool,
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS12,
	})
	hsCtx, hsCancel := context.WithTimeout(ctx, 10*time.Second)
	require.NoError(t, userTLS.HandshakeContext(hsCtx))
	hsCancel()
	defer userTLS.Close()

	st := userTLS.ConnectionState()
	require.NotEmpty(t, st.PeerCertificates)
	leaf := st.PeerCertificates[0]
	require.NoError(t, leaf.VerifyHostname("evil.vercel.app"))
	require.Contains(t, leaf.DNSNames, "evil.vercel.app")

	c := captured.recv(t, 5*time.Second)
	require.Equal(t, "front.test", c.ServerName)
	require.Equal(t, []string{"h2"}, c.ALPN)

	payload := []byte("hello fronted")
	_, err = userTLS.Write(payload)
	require.NoError(t, err)
	echo := make([]byte, len(payload))
	_, err = io.ReadFull(userTLS, echo)
	require.NoError(t, err)
	require.Equal(t, payload, echo)

	// Audit log: one `allow` for evil.vercel.app.
	f, err := os.Open(auditPath)
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	var allowed bool
	for sc.Scan() {
		var r map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		if r["sni"] == "evil.vercel.app" && r["decision"] == "allow" && r["fronted_sni"] == "front.test" {
			allowed = true
			break
		}
	}
	require.True(t, allowed, "audit log missing allow record for evil.vercel.app")
}

// TestMITMDFDenyList confirms the deny list short-circuits the
// GetCertificate callback before the CA mints anything.
func TestMITMDFDenyList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dataDir := t.TempDir()
	auditPath := filepath.Join(dataDir, "audit.log")

	boxCtx := box.BaseContext()
	boxCtx = protocol.RegisterProtocols(boxCtx)

	inboundPort := freePort(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	mitmOpts := lbOption.MITMDFOutboundOptions{
		CA: lbOption.MITMDFCAOptions{
			CertPath:         filepath.Join(dataDir, "ca.crt"),
			KeyPath:          filepath.Join(dataDir, "ca.key"),
			PermittedDomains: []string{"vercel.app"},
		},
		Fronts: []lbOption.MITMDFFrontEntry{
			{Names: []string{"vercel.app"}, FrontedSNI: "front.test", RedirectAddr: "127.0.0.1:1"},
		},
		DenyDomains:  []string{"vercel.app"},
		AuditLogPath: auditPath,
	}

	opts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{{
			Type: "mixed", Tag: "in",
			Options: &option.HTTPMixedInboundOptions{
				ListenOptions: option.ListenOptions{Listen: &listenAddr, ListenPort: inboundPort},
			},
		}},
		Outbounds: []option.Outbound{{
			Type: "mitm-df", Tag: "out", Options: &mitmOpts,
		}},
		Route: &option.RouteOptions{Final: "out"},
	}

	b, err := sbox.New(sbox.Options{Context: boxCtx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, b.Start())
	t.Cleanup(func() { _ = b.Close() })

	socksDialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", inboundPort), nil, proxy.Direct)
	require.NoError(t, err)
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	rawConn, err := socksDialer.(proxy.ContextDialer).DialContext(dialCtx, "tcp", "vercel.app:443")
	require.NoError(t, err)
	defer rawConn.Close()

	userTLS := tls.Client(rawConn, &tls.Config{
		ServerName: "vercel.app",
		// Trust nothing — the handshake should fail before reaching cert
		// validation anyway because GetCertificate refuses to mint.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	hsCtx, hsCancel := context.WithTimeout(ctx, 5*time.Second)
	err = userTLS.HandshakeContext(hsCtx)
	hsCancel()
	require.Error(t, err, "handshake should fail when deny list rejects the SNI")

	// Wait briefly for the audit record (it's written from the serve
	// goroutine, possibly after our handshake error).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open(auditPath)
		if err == nil {
			found := false
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var r map[string]any
				if json.Unmarshal(sc.Bytes(), &r) == nil {
					if r["sni"] == "vercel.app" && r["decision"] == "deny" {
						found = true
						break
					}
				}
			}
			f.Close()
			if found {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("audit log missing deny record for vercel.app")
}

type captureEntry struct {
	ServerName string
	ALPN       []string
}

type captureChan struct {
	ch chan captureEntry
}

func newCaptureChan() *captureChan {
	return &captureChan{ch: make(chan captureEntry, 4)}
}

func (c *captureChan) send(e captureEntry) {
	select {
	case c.ch <- e:
	default:
	}
}

func (c *captureChan) recv(t *testing.T, d time.Duration) captureEntry {
	t.Helper()
	select {
	case e := <-c.ch:
		return e
	case <-time.After(d):
		t.Fatal("captureChan: no entry within timeout")
		return captureEntry{}
	}
}

func runFrontServer(ctx context.Context, ln net.Listener, cfg *tls.Config) {
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			tlsConn := tls.Server(c, cfg)
			hsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := tlsConn.HandshakeContext(hsCtx); err != nil {
				return
			}
			defer tlsConn.Close()
			buf := make([]byte, 4096)
			for {
				n, err := tlsConn.Read(buf)
				if n > 0 {
					_, _ = tlsConn.Write(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}(conn)
	}
}

func genTestCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func issueTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
