package mitmca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyStore abstracts the storage of the CA's private key. The default
// implementation [FileKeyStore] writes a mode-0600 PKCS#8 PEM file. Per-
// platform hardware-backed implementations satisfy the same interface
// and live in keystore_<platform>.go behind build tags. Hardware-backed
// keys return a [crypto.Signer] whose Sign method calls into the secure
// element; the raw key bytes never leave hardware.
//
// All KeyStore methods must be safe to call from a single goroutine at a
// time. Hardware-backed implementations may serialize internally.
type KeyStore interface {
	// GenerateKey creates a fresh signing key, leaving it in a
	// not-yet-persisted state. Callers must follow up with StoreKey to
	// persist (or discard the returned signer on error paths).
	GenerateKey() (crypto.Signer, error)

	// StoreKey persists the key. For file-backed stores this writes to
	// disk with restrictive permissions. For hardware-backed stores it
	// commits the key handle to the secure element's slot.
	StoreKey(crypto.Signer) error

	// LoadKey returns the previously-stored signer, or an error wrapping
	// [os.ErrNotExist] if no key has been stored yet.
	LoadKey() (crypto.Signer, error)

	// Erase removes the stored key. Used by the uninstall flow and on CA
	// rotation. Safe to call when no key is stored.
	Erase() error
}

// FileKeyStore stores the CA's private key as a PKCS#8 PEM file with mode
// 0600. The path is supplied at construction time and is expected to live
// in the application's data dir, e.g. $DATA_DIR/mitm-ca.key.
//
// This is the default keystore on platforms that don't yet have a
// hardware-backed implementation, and the fallback when hardware-backed
// key generation fails at runtime.
//
// FileKeyStore intentionally does *not* wrap the key with an OS-level
// keyring (DPAPI, macOS Keychain item, libsecret). That layer is a
// platform-specific concern and belongs in the hardware-backed
// implementations alongside Secure Enclave / Android StrongBox / TPM —
// not bolted on here.
type FileKeyStore struct {
	Path string
	// inMemory is set after GenerateKey returns and cleared by StoreKey.
	// Lets the caller short-circuit the LoadKey path during initial setup.
	inMemory crypto.Signer
}

// NewFileKeyStore returns a FileKeyStore that reads and writes at path.
// The parent directory must already exist; we do not auto-create it
// because the path is expected to be the lantern-box data dir which is
// the caller's responsibility.
func NewFileKeyStore(path string) *FileKeyStore {
	return &FileKeyStore{Path: path}
}

// GenerateKey produces a fresh ECDSA P-256 key. The returned signer is
// also retained inside the keystore so a subsequent LoadKey (before
// StoreKey is called) returns the same key without touching disk.
func (s *FileKeyStore) GenerateKey() (crypto.Signer, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("file keystore: generate ECDSA P-256: %w", err)
	}
	s.inMemory = k
	return k, nil
}

// StoreKey writes the signer's private key to disk in PKCS#8 PEM form
// with mode 0600. Only ECDSA keys are currently accepted.
func (s *FileKeyStore) StoreKey(signer crypto.Signer) error {
	if signer == nil {
		return errors.New("file keystore: nil signer")
	}
	ek, ok := signer.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("file keystore: only ECDSA keys are supported, got %T", signer)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ek)
	if err != nil {
		return fmt.Errorf("file keystore: marshal PKCS8: %w", err)
	}
	buf := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// Write through a temp file in the same directory + rename, so a
	// crash mid-write doesn't leave a half-written key file in place.
	dir := filepath.Dir(s.Path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("file keystore: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// Best-effort cleanup if rename failed below.
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("file keystore: chmod temp: %w", err)
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("file keystore: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("file keystore: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path); err != nil {
		return fmt.Errorf("file keystore: rename %s -> %s: %w", tmpPath, s.Path, err)
	}
	s.inMemory = nil
	return nil
}

// LoadKey returns the in-memory key if one was just generated, otherwise
// reads and parses the on-disk PEM file. Wraps [os.ErrNotExist] when the
// key file is absent so callers can branch on first-run vs reuse.
func (s *FileKeyStore) LoadKey() (crypto.Signer, error) {
	if s.inMemory != nil {
		return s.inMemory, nil
	}
	buf, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("file keystore: read %s: %w", s.Path, err)
	}
	block, _ := pem.Decode(buf)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("file keystore: %s is not a PRIVATE KEY PEM", s.Path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("file keystore: parse PKCS8: %w", err)
	}
	ek, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("file keystore: stored key is not ECDSA (got %T)", key)
	}
	return ek, nil
}

// Erase deletes the on-disk key file. Returns nil if the file is already
// absent — Erase is idempotent.
func (s *FileKeyStore) Erase() error {
	s.inMemory = nil
	err := os.Remove(s.Path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("file keystore: remove %s: %w", s.Path, err)
	}
	return nil
}
