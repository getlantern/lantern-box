package exit

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCertificatePin(t *testing.T) {
	c, err := newCertificate()
	require.NoError(t, err)
	block, _ := pem.Decode([]byte(c.certPEM))
	require.NotNil(t, block)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, x509.ECDSA, leaf.PublicKeyAlgorithm)
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	assert.Equal(t, base64.StdEncoding.EncodeToString(spki[:]), c.pin)
	pin, err := Pin(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, c.pin, pin)

	other, err := newCertificate()
	require.NoError(t, err)
	assert.NotEqual(t, c.pin, other.pin, "a fresh key every time")
}
