package unbounded

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
)

// newTestCA builds a minimal self-signed ECDSA CA PEM valid enough for
// x509.CertPool.AppendCertsFromPEM. It's re-generated per test rather than
// hardcoded so we never have expiry / fingerprint drift.
func newTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestSelfSignedTLSConfig_Insecure(t *testing.T) {
	cfg, err := selfSignedTLSConfig(true, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
	if cfg.VerifyConnection != nil {
		t.Errorf("VerifyConnection should be nil in insecure mode (would reject all handshakes)")
	}
	if got := cfg.NextProtos; len(got) != 1 || got[0] != "broflake" {
		t.Errorf("NextProtos = %v, want [broflake]", got)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("want exactly 1 server cert, got %d", len(cfg.Certificates))
	}
}

func TestSelfSignedTLSConfig_SecureRequiresCA(t *testing.T) {
	if _, err := selfSignedTLSConfig(false, "", "example.com"); err == nil {
		t.Fatal("expected error when egress_ca is empty and verify is on")
	}
}

func TestSelfSignedTLSConfig_SecureRequiresServerName(t *testing.T) {
	if _, err := selfSignedTLSConfig(false, newTestCA(t), ""); err == nil {
		t.Fatal("expected error when egress_server_name is empty and verify is on")
	}
}

func TestSelfSignedTLSConfig_BadCAPem(t *testing.T) {
	_, err := selfSignedTLSConfig(false, "not a real PEM block", "example.com")
	if err == nil {
		t.Fatal("expected error for malformed CA PEM")
	}
	if !strings.Contains(err.Error(), "egress_ca") {
		t.Errorf("error should mention egress_ca, got: %v", err)
	}
}

func TestSelfSignedTLSConfig_Secure(t *testing.T) {
	cfg, err := selfSignedTLSConfig(false, newTestCA(t), "example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs should be populated from egress_ca")
	}
	if cfg.VerifyConnection == nil {
		t.Error("VerifyConnection must be set in secure mode")
	}
}

func TestVerifyPeerSAN_NoCertRejected(t *testing.T) {
	verify := verifyPeerSAN("example.com")
	if err := verify(tls.ConnectionState{}); err == nil {
		t.Fatal("expected error when peer presents no certificates")
	}
}
