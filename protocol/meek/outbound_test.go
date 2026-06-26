package meek

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"

	"github.com/getlantern/lantern-box/option"
)

// NewOutbound must reject configs that would silently weaken the
// transport: a non-https URL (which would bypass the fronted TLS dialer)
// and a front with no cert identity (which would skip hostname
// verification). Both checks run before the dialer is built, so a nil
// router/logger is fine for these error paths.
func TestNewOutbound_RejectsUnsafeConfig(t *testing.T) {
	base := func() option.MeekOutboundOptions {
		return option.MeekOutboundOptions{
			URL:    "https://meek.example/",
			Fronts: []option.FrontSpec{{IPAddress: "1.2.3.4", VerifyHostname: "a248.e.akamai.net"}},
		}
	}

	t.Run("http scheme rejected", func(t *testing.T) {
		opts := base()
		opts.URL = "http://meek.example/"
		_, err := NewOutbound(context.Background(), nil, nil, "meek", opts)
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("err = %v; want a scheme-must-be-https error", err)
		}
	})

	t.Run("front without verify_hostname or sni rejected", func(t *testing.T) {
		opts := base()
		opts.Fronts = []option.FrontSpec{{IPAddress: "1.2.3.4"}} // both SNI and VerifyHostname empty
		_, err := NewOutbound(context.Background(), nil, nil, "meek", opts)
		if err == nil || !strings.Contains(err.Error(), "verify_hostname or sni") {
			t.Errorf("err = %v; want a cert-identity-required error", err)
		}
	})

	t.Run("front with sni only is accepted past validation", func(t *testing.T) {
		opts := base()
		opts.Fronts = []option.FrontSpec{{IPAddress: "1.2.3.4", SNI: "cover.example"}}
		// Validation passes; any error must come from later dialer setup,
		// not the front/scheme guards.
		_, err := NewOutbound(context.Background(), nil, nil, "meek", opts)
		if err != nil && (strings.Contains(err.Error(), "https") || strings.Contains(err.Error(), "verify_hostname or sni")) {
			t.Errorf("sni-only front should pass the identity guard, got %v", err)
		}
	})
}

// bytewiseConn is a net.Conn that serves canned reply bytes one at a time and
// captures everything written. It reproduces the meek polling Conn's short/
// byte-wise reads — the exact pattern that desynced sing's ReadByte-based SOCKS5
// handshake (the bug this PR fixes); io.ReadFull in socks5ConnectSequenced must
// tolerate it.
type bytewiseConn struct {
	reply   []byte // server->client bytes, served one per Read
	written bytes.Buffer
}

func (c *bytewiseConn) Read(p []byte) (int, error) {
	if len(c.reply) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = c.reply[0]
	c.reply = c.reply[1:]
	return 1, nil // one byte per Read — the desync trigger
}
func (c *bytewiseConn) Write(p []byte) (int, error)      { return c.written.Write(p) }
func (c *bytewiseConn) Close() error                     { return nil }
func (c *bytewiseConn) LocalAddr() net.Addr              { return nil }
func (c *bytewiseConn) RemoteAddr() net.Addr             { return nil }
func (c *bytewiseConn) SetDeadline(time.Time) error      { return nil }
func (c *bytewiseConn) SetReadDeadline(time.Time) error  { return nil }
func (c *bytewiseConn) SetWriteDeadline(time.Time) error { return nil }

// Regression guard for FD #174614: the SOCKS5 CONNECT handshake must survive a
// Conn that returns one byte per Read (the polling-Conn behavior that made sing's
// byte-at-a-time ClientHandshake5 desync). socks5ConnectSequenced uses io.ReadFull,
// so it must complete cleanly and emit a correct no-auth method-select + CONNECT.
func TestSocks5ConnectSequenced_ToleratesBytewiseReads(t *testing.T) {
	reply := []byte{
		0x05, 0x00, // method-select: VER=5, METHOD=0x00 (no-auth)
		0x05, 0x00, 0x00, 0x01, // CONNECT reply: VER=5, REP=0x00 (ok), RSV=0x00, ATYP=IPv4
		0x00, 0x00, 0x00, 0x00, // bound addr
		0x00, 0x00, // bound port
	}
	conn := &bytewiseConn{reply: append([]byte(nil), reply...)}

	if err := socks5ConnectSequenced(conn, M.Socksaddr{Fqdn: "example.com", Port: 443}); err != nil {
		t.Fatalf("socks5ConnectSequenced over byte-wise reads: %v", err)
	}

	w := conn.written.Bytes()
	// Method-select must offer exactly no-auth: VER=5, NMETHODS=1, METHOD=0x00.
	if len(w) < 3 || w[0] != 0x05 || w[1] != 0x01 || w[2] != 0x00 {
		t.Fatalf("method-select request = %#x; want 05 01 00 prefix", w)
	}
	// A CONNECT request (VER=5, CMD=1) must follow it.
	if len(w) < 5 || w[3] != 0x05 || w[4] != 0x01 {
		t.Fatalf("connect request = %#x; want 05 01 (CONNECT) after method-select", w)
	}
}

// A non-zero RSV byte in the CONNECT reply must be rejected (RFC 1928).
func TestSocks5ConnectSequenced_RejectsNonZeroRSV(t *testing.T) {
	conn := &bytewiseConn{reply: []byte{
		0x05, 0x00, // method-select ok
		0x05, 0x00, 0x07, 0x01, // CONNECT reply with RSV=0x07 (invalid)
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}}
	err := socks5ConnectSequenced(conn, M.Socksaddr{Fqdn: "example.com", Port: 443})
	if err == nil || !strings.Contains(err.Error(), "reserved byte") {
		t.Fatalf("err = %v; want a non-zero-RSV rejection", err)
	}
}
