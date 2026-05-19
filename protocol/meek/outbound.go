package meek

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
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

// RegisterOutbound registers the meek outbound adapter.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.MeekOutboundOptions](registry, constant.TypeMeek, NewOutbound)
}

// Outbound is the sing-box outbound adapter wrapping the meek client.
type Outbound struct {
	outbound.Adapter
	logger    logger.ContextLogger
	cfg       Config
	fronts    []option.FrontSpec
	innerHost string
}

// NewOutbound constructs a meek outbound. Returns an error if no fronts
// or no URL are configured — without one, the outbound has nothing to
// dial.
func NewOutbound(
	ctx context.Context,
	router adapter.Router,
	logger log.ContextLogger,
	tag string,
	options option.MeekOutboundOptions,
) (adapter.Outbound, error) {
	if options.URL == "" {
		return nil, errors.New("meek: url is required")
	}
	if len(options.Fronts) == 0 {
		return nil, errors.New("meek: fronts is required (at least one front)")
	}
	u, err := url.Parse(options.URL)
	if err != nil {
		return nil, fmt.Errorf("meek: parse url: %w", err)
	}

	outboundDialer, err := dialer.New(ctx, options.DialerOptions, false)
	if err != nil {
		return nil, fmt.Errorf("meek: building dialer: %w", err)
	}

	connectTimeout, err := parseDurationOr(options.ConnectTimeout, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("meek: connect_timeout: %w", err)
	}
	readTimeout, err := parseDurationOr(options.ReadTimeout, defaultReadTimeout)
	if err != nil {
		return nil, fmt.Errorf("meek: read_timeout: %w", err)
	}

	pollInterval := time.Duration(options.PollIntervalMs) * time.Millisecond
	if options.PollIntervalMs <= 0 {
		pollInterval = time.Duration(defaultPollIntervalMs) * time.Millisecond
	}

	o := &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(
			constant.TypeMeek,
			tag,
			[]string{N.NetworkTCP},
			options.DialerOptions,
		),
		logger:    logger,
		fronts:    options.Fronts,
		innerHost: u.Host,
	}

	o.cfg = Config{
		URL:          options.URL,
		InnerHost:    u.Host,
		ExtraHeaders: options.Header,
		HTTPClient:   buildHTTPClient(outboundDialer, o.fronts, u, connectTimeout, readTimeout),
		PollInterval: pollInterval,
		MaxBodyBytes: options.MaxBodyBytes,
		SessionIDLen: options.SessionIDLen,
		ReadTimeout:  readTimeout,
	}
	return o, nil
}

// DialContext opens a meek-tunneled TCP connection to destination.
// destination is opaque to the meek client — bytes are forwarded
// verbatim to the meek server which handles the outbound routing.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) != N.NetworkTCP {
		return nil, fmt.Errorf("meek: unsupported network %q", network)
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	o.logger.InfoContext(ctx, "meek outbound to ", destination)
	return Dial(ctx, o.cfg)
}

// ListenPacket is unimplemented — meek is a TCP-stream-shaped transport.
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("meek: udp not supported")
}

func (o *Outbound) Network() []string { return []string{N.NetworkTCP} }

func (o *Outbound) Close() error { return nil }

// buildHTTPClient returns an *http.Client whose TCP+TLS dialer picks a
// random front from fronts on every dial and connects to its IP with
// the spec's outer SNI. cert validation uses VerifyHostname when
// present so the spec drives both who-we-look-like and who-we-trust.
func buildHTTPClient(d N.Dialer, fronts []option.FrontSpec, u *url.URL, connectTimeout, readTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			front := pickFront(fronts)
			addr := front.IPAddress
			if _, _, err := net.SplitHostPort(addr); err != nil {
				addr = net.JoinHostPort(addr, "443")
			}
			dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			raw, err := d.DialContext(dialCtx, N.NetworkTCP, M.ParseSocksaddr(addr))
			if err != nil {
				return nil, fmt.Errorf("meek: tcp dial %s: %w", addr, err)
			}
			tlsConfig := &tls.Config{InsecureSkipVerify: true}
			if front.SNI != "" {
				tlsConfig.ServerName = front.SNI
			}
			verifyHost := front.VerifyHostname
			if verifyHost == "" {
				verifyHost = front.SNI
			}
			tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return verifyChain(rawCerts, verifyHost)
			}
			conn := tls.Client(raw, tlsConfig)
			if err := conn.HandshakeContext(dialCtx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("meek: tls: %w", err)
			}
			return conn, nil
		},
		DisableKeepAlives:   false,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: connectTimeout,
	}
	return &http.Client{Transport: tr, Timeout: readTimeout}
}

func pickFront(fronts []option.FrontSpec) option.FrontSpec {
	if len(fronts) == 1 {
		return fronts[0]
	}
	return fronts[rand.IntN(len(fronts))]
}

func verifyChain(rawCerts [][]byte, dnsName string) error {
	if len(rawCerts) == 0 {
		return errors.New("no certs presented")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("parse leaf: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("system roots: %w", err)
	}
	opts := x509.VerifyOptions{
		Roots:         roots,
		DNSName:       dnsName,
		CurrentTime:   time.Now(),
		Intermediates: x509.NewCertPool(),
	}
	for i := 1; i < len(rawCerts); i++ {
		c, err := x509.ParseCertificate(rawCerts[i])
		if err != nil {
			return fmt.Errorf("intermediate %d: %w", i, err)
		}
		opts.Intermediates.AddCert(c)
	}
	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

func parseDurationOr(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}
