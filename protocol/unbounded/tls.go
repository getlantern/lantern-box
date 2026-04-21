package unbounded

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// selfSignedCertLifetime is how long the freshly-minted cert is valid.
// We regenerate on every outbound construction, so a short window is fine and
// keeps the cert out of long-term fingerprinting buckets.
const selfSignedCertLifetime = 24 * time.Hour

// selfSignedTLSConfig builds the TLS config the consumer uses when serving
// QUIC to the producer peer. Unbounded reverses the TLS role relative to TCP:
// the consumer (sing-box outbound) is the QUIC *server*, so we present a
// freshly-minted self-signed ECDSA cert and validate the peer's certificate
// via the supplied CA bundle + expected SAN.
//
// insecureDoNotVerify disables peer cert verification entirely — useful only
// for local dev and tests. In production both egressCA (PEM bundle) and
// egressServerName (expected DNS or IP SAN) must be set.
//
// Returns on every error — no panics. Nelson's earlier draft generated a
// 1024-bit RSA key and panicked on marshal errors; ECDSA P-256 produces
// smaller, faster keys and modern TLS stacks don't complain about key
// strength.
func selfSignedTLSConfig(insecureDoNotVerify bool, egressCA, egressServerName string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ECDSA key: %w", err)
	}

	// A bare template with only SerialNumber set would produce a cert with
	// year-0001 validity and no key-usage extensions, which modern TLS
	// verifiers reject as expired/invalid. Populate the minimum set a QUIC
	// server cert needs. Serial is randomized so successive certs on the
	// same host don't collide in fingerprint heuristics.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now.Add(-1 * time.Minute), // small skew tolerance
		NotAfter:              now.Add(selfSignedCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create self-signed certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ECDSA key: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return nil, fmt.Errorf("load key pair: %w", err)
	}

	clientAuth := tls.RequireAndVerifyClientCert
	if insecureDoNotVerify {
		clientAuth = tls.NoClientCert
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"broflake"},
		ClientAuth:   clientAuth,
		// QUIC requires TLS 1.3. Set explicitly rather than rely on the
		// QUIC library to enforce it, so non-QUIC uses of this config
		// (tests, debugging tools) don't silently downgrade.
		MinVersion: tls.VersionTLS13,
	}

	if insecureDoNotVerify {
		// VerifyConnection must not be set when we're explicitly not
		// requesting a peer cert — otherwise it would run on an empty
		// PeerCertificates slice and reject every handshake.
		return cfg, nil
	}

	if egressCA == "" {
		return nil, fmt.Errorf("egress_ca is required when insecure_do_not_verify_client_cert is false")
	}
	if egressServerName == "" {
		return nil, fmt.Errorf("egress_server_name is required when insecure_do_not_verify_client_cert is false")
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM([]byte(egressCA)) {
		return nil, fmt.Errorf("parse egress_ca PEM: no certificates found")
	}
	cfg.ClientCAs = caPool
	cfg.VerifyConnection = verifyPeerSAN(egressServerName)
	return cfg, nil
}

// verifyPeerSAN returns a VerifyConnection func that checks the peer's leaf
// certificate has a DNS or IP SAN matching `expected`. The x509 verifier
// already checked the chain against ClientCAs; this is the name-pin step.
func verifyPeerSAN(expected string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("egress peer did not present a certificate")
		}
		cert := cs.PeerCertificates[0]
		for _, name := range cert.DNSNames {
			if name == expected {
				return nil
			}
		}
		for _, ip := range cert.IPAddresses {
			if ip.String() == expected {
				return nil
			}
		}
		return fmt.Errorf("egress peer SAN does not include %q", expected)
	}
}
