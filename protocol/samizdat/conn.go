package samizdat

import (
	"net"

	"github.com/sagernet/sing/common/baderror"
	"github.com/sagernet/sing/common/bufio/deadline"
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
// bypass the normalization. The cost of that opacity is the
// NeedAdditionalReadDeadline hint sing-box would otherwise discover through the
// Upstream() chain, so it is forwarded explicitly.
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
