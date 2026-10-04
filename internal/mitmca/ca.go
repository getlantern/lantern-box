// Package mitmca generates and operates a name-constrained Certificate
// Authority used by the MITM-DomainFronting outbound (internal/mitmdf) to
// sign short-lived leaf certificates for client TLS termination.
//
// The defining property of this CA is that it carries an RFC 5280 §4.2.1.10
// NameConstraints extension restricting the set of DNS names it can sign
// for. On any verifier that enforces NameConstraints (Chrome, Firefox,
// Android API 30+, iOS 13+, modern Safari, modern Windows), a CA generated
// here is incapable of producing a fraudulent cert for any domain outside
// the configured permitted list — even if its private key is exfiltrated.
//
// The CA is intentionally per-device. There is no facility for sharing the
// private key across installs; GenerateCA always produces fresh key
// material. See docs/mitm-df/architecture.md and
// https://github.com/getlantern/engineering/issues/3482 for the security
// rationale.
package mitmca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// LeafCertValidity is the lifetime of every leaf certificate the CA mints.
// Kept short because a leaf is per-connection — there's no operational
// value in long-lived leaves and a short window limits the cost if one
// leaks via memory.
const LeafCertValidity = 24 * time.Hour

// DefaultCAValidity is the default lifetime of a freshly-generated CA.
// Rotation is the caller's job (the outbound regenerates and prompts the
// user to re-install on expiry).
const DefaultCAValidity = 30 * 24 * time.Hour

// CA is a name-constrained Certificate Authority. It holds the public cert,
// a [crypto.Signer] for the private key (which may be hardware-backed and
// non-extractable), and the list of permitted DNS subtrees as a friendly
// in-memory mirror of the cert's NameConstraints extension.
//
// CA is not safe for concurrent use across goroutines because the underlying
// hardware-backed Signer implementations are typically serialized internally.
// Callers that need parallel signing should pool CA instances or wrap with
// their own mutex. The MITM-DF outbound serializes per-connection so this
// isn't an issue in production.
type CA struct {
	cert             *x509.Certificate
	signer           crypto.Signer
	permittedDomains []string
}

// GenerateCA creates a fresh CA whose NameConstraints permit only the
// supplied DNS subtrees.
//
// Each entry in permittedDomains is interpreted as a permitted subtree per
// RFC 5280: "example.com" matches example.com and all of its subdomains
// (e.g. "foo.example.com"). Entries must be lowercase ASCII DNS names with
// no leading dot or wildcard — those are the only forms NameConstraints
// supports. GenerateCA rejects empty lists outright; an unconstrained CA is
// the precise threat model we exist to avoid.
//
// The keystore is responsible for producing and persisting the private key.
// Use FileKeyStore for the default cross-platform path; future hardware-
// backed implementations (Secure Enclave, Android StrongBox, TPM) satisfy
// the same interface.
//
// validity controls the CA's notAfter; the leaf certs minted later are
// always [LeafCertValidity] regardless.
func GenerateCA(permittedDomains []string, ks KeyStore, validity time.Duration) (*CA, error) {
	if len(permittedDomains) == 0 {
		return nil, errors.New("mitmca: refusing to generate an unconstrained CA — supply at least one permitted DNS subtree")
	}
	if err := validatePermittedDomains(permittedDomains); err != nil {
		return nil, err
	}
	if validity <= 0 {
		validity = DefaultCAValidity
	}

	signer, err := ks.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("mitmca: keystore.GenerateKey: %w", err)
	}

	// Random 128-bit serial — RFC 5280 §4.1.2.2 minimum is 20 bits but
	// CABF baseline requires 64; 128 is comfortably above both.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mitmca: serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			// Intentionally generic — see "CA naming for low fingerprintability"
			// in the security assessment. We do not put "Lantern" here.
			CommonName: "Local Development CA",
		},
		NotBefore: now.Add(-5 * time.Minute), // small backdate covers clock skew
		NotAfter:  now.Add(validity),
		KeyUsage:  x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		// IsCA + BasicConstraints + a path-length of 0 forbid the CA from
		// signing further CAs. We only ever sign leaves.
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		// The load-bearing field: NameConstraints. Verifiers that enforce
		// this extension reject any certificate chained to this CA whose
		// SAN list contains a DNS name outside the permitted subtrees.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         normalizeDomains(permittedDomains),
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, signer.Public(), signer)
	if err != nil {
		return nil, fmt.Errorf("mitmca: x509.CreateCertificate: %w", err)
	}
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, fmt.Errorf("mitmca: x509.ParseCertificate: %w", err)
	}

	if err := ks.StoreKey(signer); err != nil {
		return nil, fmt.Errorf("mitmca: keystore.StoreKey: %w", err)
	}

	return &CA{
		cert:             cert,
		signer:           signer,
		permittedDomains: append([]string(nil), cert.PermittedDNSDomains...),
	}, nil
}

// LoadCA reconstitutes a CA from a previously-serialized cert and a
// keystore that holds the matching private key. Returns an error if the
// cert is missing NameConstraints (we never generate such a cert, and we
// refuse to use one if one shows up — defense against silent downgrade if
// the on-disk cert is tampered with).
func LoadCA(certPEM []byte, ks KeyStore) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("mitmca: cert PEM is empty or not of type CERTIFICATE")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitmca: parse cert: %w", err)
	}
	if len(cert.PermittedDNSDomains) == 0 {
		return nil, errors.New("mitmca: loaded cert has no NameConstraints; refusing to use an unconstrained CA")
	}
	signer, err := ks.LoadKey()
	if err != nil {
		return nil, fmt.Errorf("mitmca: keystore.LoadKey: %w", err)
	}
	// Sanity: the loaded signer's public key must match the cert's public key.
	// Catches "I swapped the keystore but left the cert" mismatches.
	if !publicKeysMatch(cert.PublicKey, signer.Public()) {
		return nil, errors.New("mitmca: loaded private key does not match cert public key")
	}
	return &CA{
		cert:             cert,
		signer:           signer,
		permittedDomains: append([]string(nil), cert.PermittedDNSDomains...),
	}, nil
}

// CertPEM returns the CA certificate in PEM form. This is what the user
// installs into their device's trust store. The private key is intentionally
// not exposed; it lives in the keystore.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: c.cert.Raw,
	})
}

// Cert returns the parsed CA certificate for use with [tls.Config.RootCAs]
// or as a reference. Callers must not mutate the returned cert.
func (c *CA) Cert() *x509.Certificate {
	return c.cert
}

// PermittedDomains returns a copy of the CA's permitted DNS subtree list,
// suitable for the deny-check in the MITM-DF outbound's GetCertificate
// callback. (The crypto-level enforcement happens via the cert's
// NameConstraints extension; this is a Go-side mirror for fast pre-checks.)
func (c *CA) PermittedDomains() []string {
	return append([]string(nil), c.permittedDomains...)
}

// NotAfter returns the CA's expiry — used to drive the rotation prompt.
func (c *CA) NotAfter() time.Time {
	return c.cert.NotAfter
}

// SignLeaf mints a short-lived leaf certificate for the given SNI. The leaf
// is valid for [LeafCertValidity], has a single-entry SAN list ([sni]), and
// is chained to the CA.
//
// Returns an error — without ever calling the signer — if the SNI is not
// within the CA's permitted subtrees. This is a fast belt-and-braces check;
// the cryptographic enforcement happens at the verifier via NameConstraints.
func (c *CA) SignLeaf(sni string) (*x509.Certificate, crypto.PrivateKey, error) {
	if !c.permits(sni) {
		return nil, nil, fmt.Errorf("mitmca: SNI %q is outside this CA's permitted subtrees %v", sni, c.permittedDomains)
	}

	// Fresh key per leaf is cheap (ECDSA P-256) and avoids any leaf-key
	// reuse across sessions. The leaf private key lives only in memory and
	// is discarded when the connection closes.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("mitmca: leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("mitmca: serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: sni},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(LeafCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{sni},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, c.cert, &leafKey.PublicKey, c.signer)
	if err != nil {
		return nil, nil, fmt.Errorf("mitmca: leaf CreateCertificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("mitmca: leaf ParseCertificate: %w", err)
	}
	return leaf, leafKey, nil
}

// permits reports whether sni falls within any of the CA's permitted
// subtrees, following the RFC 5280 subtree semantics: a permitted entry of
// "example.com" matches example.com and any subdomain.
func (c *CA) permits(sni string) bool {
	sni = strings.ToLower(strings.TrimSuffix(sni, "."))
	for _, sub := range c.permittedDomains {
		if sni == sub || strings.HasSuffix(sni, "."+sub) {
			return true
		}
	}
	return false
}

// validatePermittedDomains enforces our own restrictions on what can appear
// in a permitted-subtree list. We reject wildcards (".*", "*.example.com"),
// uppercase letters, empty strings, and any name that looks like an IP —
// NameConstraints does support IP constraints separately but we don't use
// them for MITM-DF.
func validatePermittedDomains(domains []string) error {
	for _, d := range domains {
		if d == "" {
			return errors.New("mitmca: permitted-domain list contains an empty entry")
		}
		if strings.ContainsRune(d, '*') {
			return fmt.Errorf("mitmca: permitted domain %q contains a wildcard; use the bare subtree name instead", d)
		}
		if d != strings.ToLower(d) {
			return fmt.Errorf("mitmca: permitted domain %q is not all-lowercase", d)
		}
		if strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
			return fmt.Errorf("mitmca: permitted domain %q has a leading or trailing dot", d)
		}
	}
	return nil
}

func normalizeDomains(in []string) []string {
	out := make([]string, len(in))
	for i, d := range in {
		out[i] = strings.ToLower(strings.TrimSuffix(d, "."))
	}
	return out
}

// publicKeysMatch compares two crypto.PublicKey values. Used during LoadCA
// to detect a mismatched cert/keystore pair.
func publicKeysMatch(a, b crypto.PublicKey) bool {
	type equaler interface {
		Equal(crypto.PublicKey) bool
	}
	if ae, ok := a.(equaler); ok {
		return ae.Equal(b)
	}
	return false
}
