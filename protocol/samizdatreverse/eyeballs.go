package samizdatreverse

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"time"
)

// dialHappyEyeballs dials addrs IPv6 first, in the manner of RFC 8305: each
// next address starts delay after the previous one, or at once when the
// previous one fails. The first connection to succeed is returned and the
// rest are cancelled, and closed should they connect anyway. It returns as
// soon as ctx ends, without waiting for dials that ignore it.
func dialHappyEyeballs(ctx context.Context, addrs []netip.AddrPort, delay time.Duration, dial func(context.Context, netip.AddrPort) (net.Conn, error)) (net.Conn, error) {
	if len(addrs) == 0 {
		return nil, errors.New("no addresses to dial")
	}
	ordered := slices.Clone(addrs)
	slices.SortStableFunc(ordered, func(a, b netip.AddrPort) int {
		return cmp.Compare(family(a), family(b))
	})

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result, len(ordered))
	next, pending := 0, 0
	start := func() {
		addr := ordered[next]
		next++
		pending++
		go func() {
			conn, err := dial(dialCtx, addr)
			results <- result{conn, err}
		}()
	}
	// abandon closes whatever the still-pending dials return.
	abandon := func() {
		go func(n int) {
			for range n {
				if r := <-results; r.conn != nil {
					r.conn.Close()
				}
			}
		}(pending)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	start()
	var errs []error
	for {
		var fire <-chan time.Time
		if next < len(ordered) {
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			abandon()
			return nil, ctx.Err()
		case <-fire:
			start()
			timer.Reset(delay)
		case r := <-results:
			pending--
			if r.err == nil {
				cancel()
				abandon()
				return r.conn, nil
			}
			errs = append(errs, r.err)
			if next < len(ordered) {
				start()
				timer.Reset(delay)
			} else if pending == 0 {
				return nil, errors.Join(errs...)
			}
		}
	}
}

// family orders IPv6 before IPv4.
func family(a netip.AddrPort) int {
	if a.Addr().Unmap().Is4() {
		return 1
	}
	return 0
}
