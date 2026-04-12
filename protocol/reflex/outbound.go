// Package reflex implements the Reflex protocol, where TLS roles are reversed
// relative to TCP roles. The TCP client acts as the TLS server, and the TCP
// server (proxy) acts as the TLS client. This defeats SNI-based censorship and
// JA3/JA4 fingerprinting because no ClientHello appears in the client→server
// direction.
//
// Authentication is embedded in the TLS handshake itself: the client presents
// a TLS certificate whose SHA-256 fingerprint the server validates against a
// pre-shared expected value. No pre-handshake bytes are exchanged.
package reflex

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
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
	var (
		tlsCert tls.Certificate
		err     error
	)

	hasPEM := options.CertPEM != "" || options.KeyPEM != ""
	hasFile := options.CertPath != "" || options.KeyPath != ""

	if hasPEM && hasFile {
		return nil, fmt.Errorf("reflex: specify either inline PEM (cert_pem+key_pem) or file paths (cert_path+key_path), not both")
	}
	if hasPEM {
		if options.CertPEM == "" || options.KeyPEM == "" {
			return nil, fmt.Errorf("reflex: both cert_pem and key_pem must be provided together")
		}
		tlsCert, err = tls.X509KeyPair([]byte(options.CertPEM), []byte(options.KeyPEM))
	} else if hasFile {
		if options.CertPath == "" || options.KeyPath == "" {
			return nil, fmt.Errorf("reflex: both cert_path and key_path must be provided together")
		}
		tlsCert, err = tls.LoadX509KeyPair(options.CertPath, options.KeyPath)
	} else {
		return nil, fmt.Errorf("reflex: outbound requires TLS certificate (cert_pem+key_pem or cert_path+key_path)")
	}
	if err != nil {
		return nil, fmt.Errorf("reflex: loading TLS certificate: %w", err)
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

	serverAddr := M.ParseSocksaddrHostPort(o.server, o.port)
	tcpConn, err := o.dialer.DialContext(ctx, N.NetworkTCP, serverAddr)
	if err != nil {
		return nil, fmt.Errorf("reflex: TCP dial failed: %w", err)
	}

	tcpConn.SetDeadline(time.Now().Add(o.timeout))
	defer tcpConn.SetDeadline(time.Time{})

	// Act as TLS server — wait for ClientHello from the proxy.
	// No pre-handshake bytes. Auth is the certificate fingerprint.
	tlsConn := tls.Server(tcpConn, &tls.Config{
		Certificates: []tls.Certificate{o.tlsCert},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("reflex: TLS handshake failed: %w", err)
	}

	o.logger.TraceContext(ctx, "reflex connection established")

	// Send destination over encrypted channel: 2-byte big-endian length + host:port
	destStr := destination.String()
	if len(destStr) == 0 || len(destStr) > 65535 {
		tlsConn.Close()
		return nil, fmt.Errorf("reflex: destination too long (%d bytes)", len(destStr))
	}
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(destStr)))
	if _, err := tlsConn.Write(append(lenBuf[:], []byte(destStr)...)); err != nil {
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

// DERFingerprint returns the hex-encoded SHA-256 of DER-encoded certificate bytes.
func DERFingerprint(der []byte) string {
	hash := sha256.Sum256(der)
	return hex.EncodeToString(hash[:])
}
