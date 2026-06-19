package samizdat

import (
	"net"
	"testing"

	"github.com/sagernet/sing/common"
	N "github.com/sagernet/sing/common/network"
)

// halfCloseConn records whether CloseWrite reached the underlying conn.
type halfCloseConn struct {
	net.Conn
	closeWriteCalled bool
}

func (c *halfCloseConn) CloseWrite() error {
	c.closeWriteCalled = true
	return nil
}

// wrapConn deliberately blocks Upstream() to preserve H2 error normalization,
// so it must forward CloseWrite explicitly — otherwise sing-box's relay
// (bufio.CopyConn) can't detect half-close support and silently downgrades a
// half-close to a full close, tearing down request/response traffic early.
func TestWrapConnForwardsCloseWrite(t *testing.T) {
	inner := &halfCloseConn{}
	wc := &wrapConn{Conn: inner}

	// The relay gates half-close on exactly this cast.
	if _, ok := common.Cast[N.WriteCloser](wc); !ok {
		t.Fatal("wrapConn is not detectable as N.WriteCloser; half-close would be dropped")
	}
	if err := N.CloseWrite(wc); err != nil {
		t.Fatalf("CloseWrite returned error: %v", err)
	}
	if !inner.closeWriteCalled {
		t.Fatal("CloseWrite was not forwarded to the underlying stream conn")
	}
}
