package exit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// certificate is a volunteer's exit certificate: self-signed, generated at start and kept
// only in memory. Users' apps check it by its pin, which travels in each ICE answer.
//
// Users' apps check only the pin, never the validity period, so the period is not what keeps
// a long-running volunteer working. It is set to a year only so that it covers the process's
// life in practice; nothing regenerates the certificate.
type certificate struct {
	certPEM, keyPEM string
	pin             string
}

func newCertificate() (*certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	pin, err := Pin(der)
	if err != nil {
		return nil, err
	}
	return &certificate{
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		pin:     pin,
	}, nil
}

// Pin is the pin users' apps check an exit's certificate by: the standard base64 SHA-256 of
// its PKIX public key, as sing-box's certificate_public_key_sha256 takes it once decoded.
func Pin(leafDER []byte) (string, error) {
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
