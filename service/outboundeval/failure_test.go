package outboundeval

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/internal/probe"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassifyFailure(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"refused dial":     {err: fmt.Errorf("%w: %w", probe.ErrDial, syscall.ECONNREFUSED), want: failureDial},
		"unreachable dial": {err: fmt.Errorf("%w: %w", probe.ErrDial, syscall.EHOSTUNREACH), want: failureDial},
		"cancelled":        {err: fmt.Errorf("%w: %w", probe.ErrRequest, context.Canceled), want: failureCanceled},
		"expired deadline": {err: fmt.Errorf("%w: %w", probe.ErrDial, context.DeadlineExceeded), want: failureTimeout},
		"network timeout":  {err: fmt.Errorf("%w: %w", probe.ErrRequest, timeoutError{}), want: failureTimeout},
		"reset connection": {err: fmt.Errorf("%w: %w", probe.ErrRequest, syscall.ECONNRESET), want: failureConnectionReset},
		"truncated body":   {err: fmt.Errorf("%w: %w", probe.ErrResponseBody, io.ErrUnexpectedEOF), want: failureRead},
		"unparsable reply": {err: fmt.Errorf("%w: %w", probe.ErrRequest, errors.New("malformed HTTP response")), want: failureInvalidResponse},
		"untrusted issuer": {err: fmt.Errorf("%w: %w", probe.ErrRequest, x509.UnknownAuthorityError{}), want: failureTLSHandshake},
		"wrong hostname":   {err: fmt.Errorf("%w: %w", probe.ErrRequest, x509.HostnameError{}), want: failureTLSHandshake},
		"invalid certificate": {
			err:  fmt.Errorf("%w: %w", probe.ErrRequest, x509.CertificateInvalidError{}),
			want: failureTLSHandshake,
		},
		"failed verification": {
			err:  fmt.Errorf("%w: %w", probe.ErrRequest, &tls.CertificateVerificationError{}),
			want: failureTLSHandshake,
		},
		"handshake alert": {
			err:  fmt.Errorf("%w: %w", probe.ErrRequest, tls.AlertError(40)),
			want: failureTLSHandshake,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.want, classifyFailure(test.err))
		})
	}
}

// A handshake against a peer that speaks no TLS is the one classification that
// depends on how crypto/tls shapes its error, so it is produced rather than
// constructed.
func TestClassifyFailureOnARealHandshakeAgainstAPlaintextPeer(t *testing.T) {
	// The peer answers in plaintext, which is what a TLS client reads as a
	// record that is not a handshake.
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer peer.Close()
		_, _ = peer.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
		_, _ = io.Copy(io.Discard, peer)
	}()

	conn, dialErr := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, dialErr)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	err := tls.Client(conn, &tls.Config{ServerName: "measure.example"}).Handshake()

	require.Error(t, err)
	assert.Equal(t, failureTLSHandshake, classifyFailure(fmt.Errorf("%w: %w", probe.ErrRequest, err)))
}
