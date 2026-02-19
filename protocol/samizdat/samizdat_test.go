package samizdat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// testPrivKeyHex returns a random 64-char hex string (32 bytes) suitable for a private key.
func testPrivKeyHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// testPubKeyHex returns a random 64-char hex string (32 bytes) suitable for a public key.
func testPubKeyHex(t *testing.T) string {
	t.Helper()
	return testPrivKeyHex(t) // same format, just random 32 bytes
}

// testShortIDHex returns a random 16-char hex string (8 bytes).
func testShortIDHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// testCertKeyPEM generates a self-signed ECDSA cert+key pair for testing.
func testCertKeyPEM(t *testing.T) (certPEM, keyPEM string) {
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

// validInboundOptions returns a minimal valid SamizdatInboundOptions for testing.
func validInboundOptions(t *testing.T) option.SamizdatInboundOptions {
	t.Helper()
	cert, key := testCertKeyPEM(t)
	return option.SamizdatInboundOptions{
		PrivateKey:       testPrivKeyHex(t),
		ShortIDs:         []string{testShortIDHex(t)},
		CertPEM:          cert,
		KeyPEM:           key,
		MasqueradeDomain: "example.com",
	}
}

// --- NewInbound validation tests ---

func TestNewInbound_InvalidPrivateKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"not hex", "zz" + strings.Repeat("00", 31)},
		{"too short", hex.EncodeToString(make([]byte, 16))},
		{"too long", hex.EncodeToString(make([]byte, 64))},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
				PrivateKey: tt.key,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "private_key")
		})
	}
}

func TestNewInbound_InvalidShortIDs(t *testing.T) {
	privKey := testPrivKeyHex(t)

	tests := []struct {
		name        string
		ids         []string
		errContains string
	}{
		{"empty list", nil, "short_ids must contain at least one element"},
		{"empty list explicit", []string{}, "short_ids must contain at least one element"},
		{"not hex", []string{"zz" + strings.Repeat("00", 7)}, "short_ids[0]"},
		{"too short", []string{hex.EncodeToString(make([]byte, 4))}, "short_ids[0]"},
		{"too long", []string{hex.EncodeToString(make([]byte, 16))}, "short_ids[0]"},
		{"second invalid", []string{testShortIDHex(t), "bad"}, "short_ids[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
				PrivateKey: privKey,
				ShortIDs:   tt.ids,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

func TestNewInbound_CertKeyValidation(t *testing.T) {
	privKey := testPrivKeyHex(t)
	shortIDs := []string{testShortIDHex(t)}
	cert, key := testCertKeyPEM(t)

	t.Run("missing both", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TLS certificate and key must be provided")
	})

	t.Run("inline cert without key", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			CertPEM:    cert,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both cert_pem and key_pem must be provided")
	})

	t.Run("inline key without cert", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			KeyPEM:     key,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both cert_pem and key_pem must be provided")
	})

	t.Run("mixed inline and path", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			CertPEM:    cert,
			KeyPEM:     key,
			CertPath:   "/some/cert.pem",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot mix inline PEM")
	})

	t.Run("mixed inline and key path", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			CertPEM:    cert,
			KeyPEM:     key,
			KeyPath:    "/some/key.pem",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot mix inline PEM")
	})

	t.Run("cert path without key path", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			CertPath:   "/some/cert.pem",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both cert_path and key_path must be provided")
	})

	t.Run("key path without cert path", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			KeyPath:    "/some/key.pem",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both cert_path and key_path must be provided")
	})

	t.Run("cert path file not found", func(t *testing.T) {
		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			CertPath:   "/nonexistent/cert.pem",
			KeyPath:    "/nonexistent/key.pem",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading cert file")
	})

	t.Run("key path file not found", func(t *testing.T) {
		// Write a valid cert file but no key file
		dir := t.TempDir()
		certPath := filepath.Join(dir, "cert.pem")
		require.NoError(t, os.WriteFile(certPath, []byte(cert), 0o600))

		_, err := NewInbound(context.Background(), nil, nil, "test", option.SamizdatInboundOptions{
			PrivateKey: privKey,
			ShortIDs:   shortIDs,
			CertPath:   certPath,
			KeyPath:    filepath.Join(dir, "nonexistent.pem"),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading key file")
	})
}

func TestNewInbound_InvalidTimeouts(t *testing.T) {
	opts := validInboundOptions(t)

	t.Run("bad masquerade_idle_timeout", func(t *testing.T) {
		o := opts
		o.MasqueradeIdleTimeout = "not-a-duration"
		_, err := NewInbound(context.Background(), nil, nil, "test", o)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "masquerade_idle_timeout")
	})

	t.Run("bad masquerade_max_duration", func(t *testing.T) {
		o := opts
		o.MasqueradeMaxDuration = "not-a-duration"
		_, err := NewInbound(context.Background(), nil, nil, "test", o)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "masquerade_max_duration")
	})
}

// --- NewOutbound validation tests ---

func TestNewOutbound_InvalidPublicKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"not hex", "zz" + strings.Repeat("00", 31)},
		{"too short", hex.EncodeToString(make([]byte, 16))},
		{"too long", hex.EncodeToString(make([]byte, 64))},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOutbound(context.Background(), nil, nil, "test", option.SamizdatOutboundOptions{
				PublicKey: tt.key,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "public_key")
		})
	}
}

func TestOutbound_NetworkIncludesUDP(t *testing.T) {
	o := &Outbound{}
	networks := o.Network()
	assert.Contains(t, networks, "tcp", "Network() should include TCP")
	assert.Contains(t, networks, "udp", "Network() should include UDP")
	assert.Len(t, networks, 2, "Network() should return exactly 2 networks")
}

func TestNewOutbound_InvalidShortID(t *testing.T) {
	pubKey := testPubKeyHex(t)

	tests := []struct {
		name string
		id   string
	}{
		{"not hex", "zz" + strings.Repeat("00", 7)},
		{"too short", hex.EncodeToString(make([]byte, 4))},
		{"too long", hex.EncodeToString(make([]byte, 16))},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOutbound(context.Background(), nil, nil, "test", option.SamizdatOutboundOptions{
				PublicKey: pubKey,
				ShortID:   tt.id,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "short_id")
		})
	}
}
