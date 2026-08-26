// Package masquerade implements the splitting egress: a connection that fails
// to authenticate is forwarded verbatim to a real cover site, which answers it.
//
// For protocols that cannot complete a genuine handshake with an unauthenticated
// peer -- reflex, and twiddle, whose TLS opening is synthesised -- this is not an
// optional hardening. It is the only thing standing between an active prober and
// a distinguishing reply.
package masquerade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// dialTimeout is the timeout for dialing the upstream cover site.
const dialTimeout = 10 * time.Second

// Blocks until both copy directions return. Returns the first non-EOF copy
// error, or the dial/replay error, or nil on clean close.
//
// Blocks until both copy directions return. Returns the first non-EOF copy
// error, or the dial/replay error, or nil on clean close.
//
// To unblock the copy loops on context cancellation and when either
// direction finishes, this function may close both the upstream connection
// and conn. Callers should treat conn as possibly-closed after return.
// Forward transparently forwards conn to upstream (host:port), prepending any
// prefix bytes already consumed from conn.
func Forward(ctx context.Context, conn net.Conn, upstream string, prefix []byte) error {
	if upstream == "" {
		return fmt.Errorf("masquerade upstream not configured")
	}

	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var d net.Dialer
	upstreamConn, err := d.DialContext(dctx, "tcp", upstream)
	if err != nil {
		return fmt.Errorf("dial masquerade upstream %s: %w", upstream, err)
	}
	defer upstreamConn.Close()

	// Wire ctx cancellation to close both sides so copies unblock and return.
	stop := context.AfterFunc(ctx, func() {
		_ = upstreamConn.Close()
		_ = conn.Close()
	})
	defer stop()

	// Replay the byte(s) we consumed during silence detection so the upstream
	// sees the client's stream unmodified.
	if len(prefix) > 0 {
		if _, err := upstreamConn.Write(prefix); err != nil {
			return fmt.Errorf("replay prefix to upstream: %w", err)
		}
	}

	// Bidirectional copy. When one direction ends, close the other so the
	// second goroutine unblocks. Then return the first real error.
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstreamConn, conn)
		_ = upstreamConn.Close()
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(conn, upstreamConn)
		_ = conn.Close()
		errCh <- err
	}()
	return FirstRealError(<-errCh, <-errCh)
}

// FirstRealError returns the first non-nil, non-EOF, non-closed-network error.
func FirstRealError(errs ...error) error {
	for _, err := range errs {
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			continue
		}
		return err
	}
	return nil
}
