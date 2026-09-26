package outboundeval

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"

	"github.com/getlantern/lantern-box/internal/probe"
)

// Failure codes reported for one attempt. They describe how far the fetch got
// and nothing about the path it took, since a runner is never told what it is
// measuring.
const (
	failureDial            = "dial_failed"
	failureTLSHandshake    = "tls_handshake_failed"
	failureTimeout         = "timeout"
	failureConnectionReset = "connection_reset"
	failureRead            = "read_failed"
	failureInvalidResponse = "invalid_response"
	failureHTTPStatus      = "http_status"
	failureCanceled        = "canceled"
)

// Failure codes reported for every attempt of a window that could not be
// measured at all. They fill the window so the grid stays complete, and they
// fill both arms, which keeps the window from reading as evidence against the
// candidate.
const (
	failureAttestation         = "attestation_failed"
	failureAttestationRejected = "attestation_rejected"
	failureWindowDeadline      = "window_deadline_exceeded"
)

// classifyFailure maps a probe failure onto a stable code. Handshake and reset
// errors are classified ahead of the stage they surfaced at, because a TLS
// rejection during a request says more than "the request failed".
func classifyFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return failureCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return failureTimeout
	case isTLSFailure(err):
		return failureTLSHandshake
	case errors.Is(err, syscall.ECONNRESET):
		return failureConnectionReset
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return failureTimeout
	}
	switch {
	case errors.Is(err, probe.ErrDial):
		return failureDial
	case errors.Is(err, probe.ErrResponseBody):
		return failureRead
	default:
		return failureInvalidResponse
	}
}

func isTLSFailure(err error) bool {
	var (
		// crypto/tls returns a RecordHeaderError by value and a
		// CertificateVerificationError by pointer.
		recordHeader  tls.RecordHeaderError
		verification  *tls.CertificateVerificationError
		unknownCA     x509.UnknownAuthorityError
		invalidCert   x509.CertificateInvalidError
		wrongHostname x509.HostnameError
		alert         tls.AlertError
	)
	return errors.As(err, &recordHeader) || errors.As(err, &verification) ||
		errors.As(err, &unknownCA) || errors.As(err, &invalidCert) ||
		errors.As(err, &wrongHostname) || errors.As(err, &alert)
}
