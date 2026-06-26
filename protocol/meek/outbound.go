package meek

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
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
	"github.com/sagernet/sing/protocol/socks/socks5"

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
	logger         logger.ContextLogger
	cfg            Config
	fronts         []option.FrontSpec
	connectTimeout time.Duration
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
	// Must be https: the transport routes every dial through
	// DialTLSContext (the fronted TLS dialer). An http:// URL would make
	// http.Transport use DialContext instead, bypassing fronting and the
	// cert pinning below — silently leaking traffic.
	if u.Scheme != "https" {
		return nil, fmt.Errorf("meek: url scheme must be https, got %q", u.Scheme)
	}
	// Every front must pin a cert identity. verifyChain runs with
	// VerifyHostname (falling back to SNI); if both are empty the cert
	// check degrades to "any publicly-trusted cert", which is no check at
	// all for a fronted endpoint. Reject at config time rather than
	// silently accepting MITM.
	for i, f := range options.Fronts {
		if f.VerifyHostname == "" && f.SNI == "" {
			return nil, fmt.Errorf("meek: fronts[%d] must set verify_hostname or sni so the cert identity can be checked", i)
		}
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
		logger:         logger,
		fronts:         options.Fronts,
		connectTimeout: connectTimeout,
	}

	o.cfg = Config{
		URL:          options.URL,
		InnerHost:    u.Host,
		ExtraHeaders: options.Header,
		HTTPClient:   buildHTTPClient(outboundDialer, o.fronts, connectTimeout, readTimeout),
		PollInterval: pollInterval,
		MaxBodyBytes: options.MaxBodyBytes,
		SessionIDLen: options.SessionIDLen,
		ReadTimeout:  readTimeout,
	}
	return o, nil
}

// DialContext opens a meek-tunneled TCP connection to destination.
//
// sing-box treats this as a terminal outbound and writes the application
// stream straight into the returned conn, so the destination must be
// conveyed to the meek server's upstream before we hand the conn back.
// That upstream is a SOCKS5 proxy (microsocks in the standard
// deployment), so we run a SOCKS5 CONNECT to destination over the tunnel
// first; without it the upstream would read the application's opening
// bytes as a malformed SOCKS handshake and the connection would fail.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) != N.NetworkTCP {
		return nil, fmt.Errorf("meek: unsupported network %q", network)
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	o.logger.InfoContext(ctx, "meek outbound to ", destination)

	conn, err := Dial(ctx, o.cfg)
	if err != nil {
		return nil, err
	}
	// Bound the handshake: meek reads have no native ctx deadline, so a
	// silent upstream would otherwise park here until the poll loop's own
	// read timeout.
	_ = conn.SetDeadline(time.Now().Add(o.connectTimeout))
	if err := socks5ConnectSequenced(conn, destination); err != nil {
		conn.Close()
		return nil, fmt.Errorf("meek: socks5 connect to %s: %w", destination, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// socks5ConnectSequenced performs the SOCKS5 no-auth CONNECT handshake over the
// meek Conn with reads done via io.ReadFull. sing's socks.ClientHandshake5 reads
// the replies byte-at-a-time through varbin's stub ReadByte (which issues a
// 1-byte Read and ignores n); over the meek polling Conn that desyncs the
// handshake — DialContext returns instantly while microsocks actually replies
// 0x05 0xFF ("no acceptable methods"), and that rejection leaks into the
// application stream so every transfer stalls. io.ReadFull tolerates short/zero
// reads and the explicit method-select→reply→CONNECT→reply ordering matches what
// microsocks requires (strict, no pipelining).
func socks5ConnectSequenced(conn net.Conn, dst M.Socksaddr) error {
	// 1. Offer only NO_AUTH, then read the 2-byte method-select reply.
	if err := socks5.WriteAuthRequest(conn, socks5.AuthRequest{Methods: []byte{socks5.AuthTypeNotRequired}}); err != nil {
		return fmt.Errorf("write method-select: %w", err)
	}
	authReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, authReply); err != nil {
		return fmt.Errorf("read method-select reply: %w", err)
	}
	if authReply[0] != socks5.Version {
		return fmt.Errorf("unexpected socks version %#x", authReply[0])
	}
	if authReply[1] != socks5.AuthTypeNotRequired {
		return fmt.Errorf("server rejected no-auth (method %#x)", authReply[1])
	}

	// 2. CONNECT, then read the reply: 4-byte header + bound addr + 2-byte port.
	if err := socks5.WriteRequest(conn, socks5.Request{Command: socks5.CommandConnect, Destination: dst}); err != nil {
		return fmt.Errorf("write connect: %w", err)
	}
	head := make([]byte, 4) // VER, REP, RSV, ATYP
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("read connect reply: %w", err)
	}
	if head[0] != socks5.Version {
		return fmt.Errorf("unexpected socks version %#x in connect reply", head[0])
	}
	if head[1] != 0 { // 0x00 = succeeded (RFC 1928)
		return fmt.Errorf("connect failed (reply code %#x)", head[1])
	}
	if head[2] != 0 { // RSV must be 0x00 (RFC 1928)
		return fmt.Errorf("unexpected reserved byte %#x in connect reply", head[2])
	}
	var addrLen int
	switch head[3] { // ATYP (RFC 1928): 1=IPv4, 3=domain, 4=IPv6
	case 0x01:
		addrLen = net.IPv4len
	case 0x04:
		addrLen = net.IPv6len
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			return fmt.Errorf("read bound-addr length: %w", err)
		}
		addrLen = int(lb[0])
	default:
		return fmt.Errorf("unexpected atyp %#x in connect reply", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil { // bound addr + port
		return fmt.Errorf("read bound addr/port: %w", err)
	}
	return nil
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
func buildHTTPClient(d N.Dialer, fronts []option.FrontSpec, connectTimeout, readTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			front := pickFront(fronts)
			addr := front.IPAddress
			if _, _, err := net.SplitHostPort(addr); err != nil {
				addr = net.JoinHostPort(addr, "443")
			}
			verifyHost := front.VerifyHostname
			if verifyHost == "" {
				verifyHost = front.SNI
			}
			// NewOutbound rejects fronts with neither set, but guard the
			// dial too: an empty DNSName makes verifyChain skip the
			// hostname check, which would silently accept any trusted cert.
			if verifyHost == "" {
				return nil, errors.New("meek: front has no verify_hostname or sni; refusing to dial without cert identity")
			}
			dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			raw, err := d.DialContext(dialCtx, N.NetworkTCP, M.ParseSocksaddr(addr))
			if err != nil {
				return nil, fmt.Errorf("meek: tcp dial %s: %w", addr, err)
			}
			// InsecureSkipVerify disables the default CA+hostname check so
			// we can run our own via VerifyPeerCertificate against the
			// front's pinned identity (verifyHost) rather than the outer
			// SNI — the SNI is cover, not the cert we trust.
			tlsConfig := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // custom verification below pins verifyHost
			if front.SNI != "" {
				tlsConfig.ServerName = front.SNI
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
