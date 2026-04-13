package reflex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// masqueradeDialTimeout is the timeout for dialing the upstream cover site.
const masqueradeDialTimeout = 10 * time.Second

// forwardToMasquerade transparently forwards conn to upstream (host:port),
// prepending prefix bytes (which were already consumed from conn during the
// silence probe) before the client's stream.
//
// Blocks until both copy directions return. Returns the first non-EOF copy
// error, or the dial/replay error, or nil on clean close. Context
// cancellation closes both conns so copies unblock. conn is not closed by
// this function — the caller owns conn's lifecycle.
func forwardToMasquerade(ctx context.Context, conn net.Conn, upstream string, prefix []byte) error {
	if upstream == "" {
		return fmt.Errorf("masquerade upstream not configured")
	}

	dctx, cancel := context.WithTimeout(ctx, masqueradeDialTimeout)
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
	return firstRealError(<-errCh, <-errCh)
}

// firstRealError returns the first non-nil, non-EOF, non-closed-network error
// from the two copy directions. EOF and use-of-closed-connection are expected
// on clean shutdown.
func firstRealError(errs ...error) error {
	for _, err := range errs {
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			continue
		}
		return err
	}
	return nil
}
