package mitmca

import (
	"crypto/ecdsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newCAForTest(t *testing.T, permitted []string) *CA {
	t.Helper()
	ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))
	ca, err := GenerateCA(permitted, ks, DefaultCAValidity)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return ca
}

func TestGenerateCA_RejectsEmptyPermitted(t *testing.T) {
	ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))
	if _, err := GenerateCA(nil, ks, DefaultCAValidity); err == nil {
		t.Fatal("expected error for empty permitted-domains list, got nil")
	}
}

func TestGenerateCA_RejectsBadPermitted(t *testing.T) {
	cases := []struct {
		name    string
		domains []string
	}{
		{"wildcard", []string{"*.example.com"}},
		{"uppercase", []string{"Example.com"}},
		{"leading-dot", []string{".example.com"}},
		{"trailing-dot", []string{"example.com."}},
		{"empty-entry", []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))
			if _, err := GenerateCA(tc.domains, ks, DefaultCAValidity); err == nil {
				t.Fatalf("expected error for %v, got nil", tc.domains)
			}
		})
	}
}

func TestGenerateCA_SetsNameConstraints(t *testing.T) {
	ca := newCAForTest(t, []string{"example.com", "vercel.app"})
	cert := ca.Cert()

	if !cert.PermittedDNSDomainsCritical {
		t.Error("NameConstraints extension is not marked critical")
	}
	if len(cert.PermittedDNSDomains) != 2 {
		t.Errorf("PermittedDNSDomains = %v, want 2 entries", cert.PermittedDNSDomains)
	}
	if !cert.IsCA || cert.MaxPathLen != 0 {
		t.Errorf("expected IsCA=true MaxPathLen=0, got IsCA=%v MaxPathLen=%d", cert.IsCA, cert.MaxPathLen)
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("missing KeyUsageCertSign")
	}
}

func TestGenerateCA_DefaultsValidityWhenNonPositive(t *testing.T) {
	ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))
	ca, err := GenerateCA([]string{"example.com"}, ks, 0)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	gotValidity := time.Until(ca.NotAfter()).Round(time.Hour)
	wantValidity := DefaultCAValidity.Round(time.Hour)
	if abs(gotValidity-wantValidity) > time.Hour {
		t.Errorf("validity = %v, want ~%v", gotValidity, wantValidity)
	}
}

func TestSignLeaf_PermittedSNI(t *testing.T) {
	ca := newCAForTest(t, []string{"example.com"})

	for _, sni := range []string{"example.com", "foo.example.com", "deep.sub.example.com"} {
		t.Run(sni, func(t *testing.T) {
			leaf, _, err := ca.SignLeaf(sni)
			if err != nil {
				t.Fatalf("SignLeaf(%q): %v", sni, err)
			}
			if leaf.Subject.CommonName != sni {
				t.Errorf("leaf CN = %q, want %q", leaf.Subject.CommonName, sni)
			}
			if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != sni {
				t.Errorf("leaf DNSNames = %v, want [%q]", leaf.DNSNames, sni)
			}
			lifetime := time.Until(leaf.NotAfter).Round(time.Minute)
			if abs(lifetime-LeafCertValidity) > 5*time.Minute {
				t.Errorf("leaf lifetime = %v, want ~%v", lifetime, LeafCertValidity)
			}
		})
	}
}

func TestSignLeaf_RejectsOutsidePermitted(t *testing.T) {
	ca := newCAForTest(t, []string{"example.com"})

	// Includes the classic gotchas: "notexample.com" ends with "example.com"
	// lexically but is not under the subtree; "example.com.evil.com" sits
	// under a different (evil.com) subtree entirely.
	cases := []string{
		"otherdomain.com",
		"evil.com",
		"chase.com",
		"gov.ir",
		"example.com.evil.com",
		"notexample.com",
	}
	for _, sni := range cases {
		t.Run(sni, func(t *testing.T) {
			_, _, err := ca.SignLeaf(sni)
			if err == nil {
				t.Fatalf("SignLeaf(%q) succeeded; expected permitted-subtree error", sni)
			}
		})
	}
}

func TestSignLeaf_VerifiesUnderCAPool(t *testing.T) {
	// Confirms that a leaf we mint validates under the CA's pool when the
	// SNI is inside permitted subtrees. This implicitly exercises that
	// Go's stdlib NameConstraints enforcement accepts our cert (which we'd
	// want to break if we ever accidentally produced an inconsistent
	// constraint encoding).
	ca := newCAForTest(t, []string{"example.com"})

	leaf, _, err := ca.SignLeaf("ok.example.com")
	if err != nil {
		t.Fatalf("SignLeaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert())
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		DNSName:     "ok.example.com",
		CurrentTime: time.Now(),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("verify of legitimate leaf failed: %v", err)
	}
}

func TestLoadCA_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	ks := NewFileKeyStore(filepath.Join(dir, "ca.key"))

	caA, err := GenerateCA([]string{"example.com"}, ks, DefaultCAValidity)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certPEM := caA.CertPEM()

	// Fresh keystore pointing at the same file: simulates a process restart.
	ks2 := NewFileKeyStore(ks.Path)
	caB, err := LoadCA(certPEM, ks2)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if caB.Cert().SerialNumber.Cmp(caA.Cert().SerialNumber) != 0 {
		t.Error("loaded CA has a different serial number — keystore/cert pair mismatched?")
	}
	if _, _, err := caB.SignLeaf("foo.example.com"); err != nil {
		t.Errorf("reloaded CA cannot sign: %v", err)
	}
}

func TestLoadCA_RejectsInvalidPEM(t *testing.T) {
	ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))
	if _, err := LoadCA([]byte("not a pem"), ks); err == nil {
		t.Fatal("LoadCA accepted non-PEM input; want error")
	}
}

func TestFileKeyStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.key")
	ks := NewFileKeyStore(path)

	signer, err := ks.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ek, ok := signer.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("GenerateKey returned %T; want *ecdsa.PrivateKey", signer)
	}
	if err := ks.StoreKey(signer); err != nil {
		t.Fatalf("StoreKey: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("file mode = %v, want 0o600", mode)
		}
	}

	// Reload (fresh keystore, same path) and confirm the public key matches.
	ks2 := NewFileKeyStore(path)
	loaded, err := ks2.LoadKey()
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if !ek.PublicKey.Equal(loaded.Public()) {
		t.Error("reloaded public key differs from original")
	}
}

func TestFileKeyStore_LoadBeforeStoreReturnsInMemory(t *testing.T) {
	ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))
	signer, err := ks.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	loaded, err := ks.LoadKey()
	if err != nil {
		t.Fatalf("LoadKey (before StoreKey): %v", err)
	}
	if signer != loaded {
		t.Error("LoadKey returned a different signer than GenerateKey")
	}
}

func TestFileKeyStore_EraseIsIdempotent(t *testing.T) {
	ks := NewFileKeyStore(filepath.Join(t.TempDir(), "ca.key"))

	// Erase with no file present: succeeds.
	if err := ks.Erase(); err != nil {
		t.Errorf("Erase on empty keystore: %v", err)
	}

	// Generate + store + erase.
	signer, err := ks.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := ks.StoreKey(signer); err != nil {
		t.Fatalf("StoreKey: %v", err)
	}
	if err := ks.Erase(); err != nil {
		t.Errorf("Erase after StoreKey: %v", err)
	}
	if _, err := os.Stat(ks.Path); !os.IsNotExist(err) {
		t.Errorf("key file still present after Erase: %v", err)
	}

	// Erase again: still succeeds (idempotent).
	if err := ks.Erase(); err != nil {
		t.Errorf("second Erase: %v", err)
	}
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
