package samizdat

import (
	"net"

	"github.com/sagernet/sing/common/baderror"
	"github.com/sagernet/sing/common/bufio/deadline"
	N "github.com/sagernet/sing/common/network"
)

// wrapConn normalizes HTTP/2 read errors on a samizdat stream conn.
//
// The samizdat transport carries proxied traffic over HTTP/2 streams, so a
// torn-down stream surfaces golang.org/x/net/http2 errors such as
// "http2: response body closed" or "; CANCEL". baderror.WrapH2 maps those to
// net.ErrClosed so the router treats them as an ordinary connection close
// rather than a transport fault.
//
// UoT packet conns are not wrapped directly: they read through the samizdat
// stream conn returned by samizdatDialer.DialContext, which is wrapped here,
// so their read errors are already normalized at the stream layer.
//
// wrapConn is deliberately opaque: it does not expose Upstream(), so sing-box
// cannot unwrap past it and read the underlying conn directly, which would
// bypass the normalization. The cost of that opacity is that capabilities
// sing-box would otherwise discover through the Upstream() chain must be
// forwarded explicitly: the NeedAdditionalReadDeadline hint and TCP half-close
// (CloseWrite), below.
type wrapConn struct {
	net.Conn
}

func (c *wrapConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, baderror.WrapH2(err)
}

func (c *wrapConn) NeedAdditionalReadDeadline() bool {
	return deadline.NeedAdditionalReadDeadline(c.Conn)
}

// CloseWrite forwards TCP half-close to the underlying samizdat stream conn,
// which maps it to an HTTP/2 END_STREAM (request side closed, response side
// kept open). Without it, wrapConn hides the stream's CloseWrite (Upstream is
// blocked above) and the sing-box relay silently downgrades half-close to a
// full close.
func (c *wrapConn) CloseWrite() error {
	return N.CloseWrite(c.Conn)
}
