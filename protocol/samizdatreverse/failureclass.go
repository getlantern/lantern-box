package samizdatreverse

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	sbdns "github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/route/rule"
	E "github.com/sagernet/sing/common/exceptions"
)

// failureClass names why a routed connection failed, from a closed set of
// values that never include the destination, an address or error text.
func failureClass(err error) string {
	if rule.IsRejected(err) {
		return "rejected"
	}
	// Not E.IsCanceled, which also matches context.DeadlineExceeded.
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) || anyTimeout(err) {
		return "timeout"
	}
	var dnsErr *net.DNSError
	var rcodeErr sbdns.RcodeError
	if errors.As(err, &dnsErr) || errors.As(err, &rcodeErr) {
		return "dns"
	}
	switch {
	case isErrno(err, refusedErrnos):
		return "refused"
	case isErrno(err, unreachableErrnos):
		return "unreachable"
	case isErrno(err, resetErrnos):
		return "reset"
	case isErrno(err, notPermittedErrnos):
		return "not-permitted"
	}
	if E.IsClosed(err) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return "closed"
	}
	return "other"
}

// anyTimeout reports whether any error in err's tree is a net.Error that
// timed out. errors.As would stop at the first net.Error, which in an error
// joined from parallel dials can be a refusal ahead of the timeout.
func anyTimeout(err error) bool {
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	switch x := err.(type) {
	case interface{ Unwrap() error }:
		return anyTimeout(x.Unwrap())
	case interface{ Unwrap() []error }:
		for _, inner := range x.Unwrap() {
			if anyTimeout(inner) {
				return true
			}
		}
	}
	return false
}

func isErrno(err error, errnos []syscall.Errno) bool {
	for _, errno := range errnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
