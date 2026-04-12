// Package reflex implements the Reflex protocol, where TLS roles are reversed
// relative to TCP roles. The TCP client acts as the TLS server, and the TCP
// server (proxy) acts as the TLS client. This defeats SNI-based censorship and
// JA3/JA4 fingerprinting because no ClientHello appears in the client→server
// direction.
//
// Authentication is embedded in the TLS handshake itself: the client presents
// a certificate whose SHA-256 fingerprint the server validates against a
// pre-shared expected value. No pre-handshake auth bytes are sent.
package reflex

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.ReflexOutboundOptions](registry, constant.TypeReflex, NewOutbound)
}

// Outbound implements the client side of Reflex. It dials a TCP connection,
// then acts as a TLS server (waits for ClientHello from the proxy). The server
// validates the client's TLS certificate fingerprint as authentication.
// No pre-handshake bytes are sent — the entire auth is within the TLS handshake.
type Outbound struct {
	outbound.Adapter
	logger  logger.ContextLogger
	dialer  N.Dialer
	server  string
	port    uint16
	tlsCert tls.Certificate
	timeout time.Duration
}

func NewOutbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.ReflexOutboundOptions) (adapter.Outbound, error) {
	// Load TLS certificate (client acts as TLS server, so needs a cert).
	// The SHA-256 fingerprint of this cert is the auth token — the server
	// validates it during the handshake.
	var (
		tlsCert tls.Certificate
		err     error
	)
	if options.CertPEM != "" && options.KeyPEM != "" {
		tlsCert, err = tls.X509KeyPair([]byte(options.CertPEM), []byte(options.KeyPEM))
		if err != nil {
			return nil, fmt.Errorf("reflex: invalid inline cert/key: %w", err)
		}
	} else if options.CertPath != "" && options.KeyPath != "" {
		tlsCert, err = tls.LoadX509KeyPair(options.CertPath, options.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("reflex: failed to load cert/key files: %w", err)
		}
	} else {
		return nil, fmt.Errorf("reflex: outbound requires TLS certificate (cert_pem+key_pem or cert_path+key_path)")
	}

	timeout := 15 * time.Second
	if options.ConnectTimeout != "" {
		timeout, err = time.ParseDuration(options.ConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("reflex: invalid connect_timeout: %w", err)
		}
	}

	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(constant.TypeReflex, tag, []string{N.NetworkTCP}, options.DialerOptions),
		logger:  lg,
		dialer:  outboundDialer,
		server:  options.Server,
		port:    options.ServerPort,
		tlsCert: tlsCert,
		timeout: timeout,
	}, nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if network != N.NetworkTCP {
		return nil, fmt.Errorf("reflex: only TCP is supported")
	}

	o.logger.TraceContext(ctx, "dialing reflex connection to ", o.server, ":", o.port)

	// 1. Establish TCP connection to proxy server
	serverAddr := M.ParseSocksaddrHostPort(o.server, o.port)
	tcpConn, err := o.dialer.DialContext(ctx, N.NetworkTCP, serverAddr)
	if err != nil {
		return nil, fmt.Errorf("reflex: TCP dial failed: %w", err)
	}

	tcpConn.SetDeadline(time.Now().Add(o.timeout))
	defer tcpConn.SetDeadline(time.Time{})

	// 2. Act as TLS server — wait for ClientHello from the proxy.
	// No pre-handshake bytes sent. Auth is the certificate fingerprint.
	tlsConn := tls.Server(tcpConn, &tls.Config{
		Certificates: []tls.Certificate{o.tlsCert},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("reflex: TLS handshake failed: %w", err)
	}

	o.logger.TraceContext(ctx, "reflex connection established")

	// 3. Send destination over the encrypted channel
	destStr := destination.String()
	destLen := byte(len(destStr))
	if _, err := tlsConn.Write(append([]byte{destLen}, []byte(destStr)...)); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("reflex: failed to send destination: %w", err)
	}

	return tlsConn, nil
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("reflex: UDP not supported")
}

func (o *Outbound) Network() []string {
	return []string{N.NetworkTCP}
}

func (o *Outbound) Close() error {
	return nil
}

// CertFingerprint returns the SHA-256 fingerprint of the outbound's TLS
// certificate. This is what the server uses to authenticate the client.
func CertFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	hash := sha256.Sum256(cert.Certificate[0])
	return fmt.Sprintf("%x", hash)
}
