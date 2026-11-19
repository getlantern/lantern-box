// Package quicjunk generates the random datagram a QUIC client sends before its
// Initial, so the Initial is never a flow's first datagram.
package quicjunk

import (
	"crypto/rand"
	"io"
	"math/big"
)

const (
	minJunkLen = 8
	maxJunkLen = 64
)

// New returns 8–64 random bytes with the QUIC long-header bit set. quic-go
// drops long-header packets under 1200 bytes with an unsupported version before
// it considers version negotiation, and drops the ~1-in-2^32 all-zero-version
// case as a Version Negotiation packet, so the server never answers the junk.
func New() ([]byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(maxJunkLen-minJunkLen+1))
	if err != nil {
		return nil, err
	}
	junk := make([]byte, minJunkLen+int(n.Int64()))
	if _, err := rand.Read(junk); err != nil {
		return nil, err
	}
	junk[0] |= 0x80
	return junk, nil
}

// Write writes one New datagram to conn, a flow's UDP conn before QUIC takes it over.
func Write(conn io.Writer) error {
	junk, err := New()
	if err != nil {
		return err
	}
	_, err = conn.Write(junk)
	return err
}
