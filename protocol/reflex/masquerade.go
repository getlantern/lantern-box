package reflex

import (
	"context"
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
// Blocks until one direction of the copy returns. conn is not closed by this
// function — the caller owns conn's lifecycle.
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

	// Replay the byte(s) we consumed during silence detection so the upstream
	// sees the client's stream unmodified.
	if len(prefix) > 0 {
		if _, err := upstreamConn.Write(prefix); err != nil {
			return fmt.Errorf("replay prefix to upstream: %w", err)
		}
	}

	// Bidirectional copy. Exit when either direction closes.
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstreamConn, conn)
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(conn, upstreamConn)
		errCh <- err
	}()
	<-errCh
	return nil
}
