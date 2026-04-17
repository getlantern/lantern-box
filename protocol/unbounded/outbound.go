// Package unbounded implements a consumer-side Unbounded outbound for
// sing-box.
//
// Unbounded is Lantern's WebRTC-based peer-to-peer circumvention system. A
// volunteer "producer" (a browser tab running the Unbounded widget) pairs
// with a "consumer" (this outbound). The consumer sends QUIC-over-data-channel
// to the producer, which forwards it to an egress server that speaks plain
// internet.
//
// # Architecture in the refactored radiance
//
// The outbound lives entirely inside the VPN tunnel process (the Android VPN
// service, the iOS/macOS NetworkExtension, the Windows service, or the Linux
// daemon). That makes the plumbing simpler than the split-process design:
//
//   - broflake + pion run in the same process that owns the TUN
//   - signaling to freddie goes out the physical interface because the
//     hosting process injects a kindling-backed RoundTripper via
//     adapter.ContextWithDirectTransport
//   - STUN/DTLS to the peer goes through the sing-box dialer — we bind
//     straight to the physical interface via the direct outbound, which in
//     turn applies VpnService.protect / IP_BOUND_IF automatically
//
// # Options
//
// option.UnboundedOutboundOptions holds only JSON-serializable fields.
// Runtime-only deps (HTTP transport for signaling, logger) come in via the
// sing-box logger interface and the ctx, respectively.
package unbounded

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
	C "github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// RegisterOutbound registers the Unbounded outbound with the given sing-box
// OutboundRegistry. The hosting process should call this once at startup,
// before libbox.NewServiceWithContext.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.UnboundedOutboundOptions](registry, C.TypeUnbounded, NewOutbound)
}

// Outbound is the consumer-side Unbounded outbound.
//
// Construction (NewOutbound) collects options and produces a zero-value
// broflake tree. The WebRTC + QUIC machinery doesn't spin up until Start is
// called at PostStart — that's when the tunnel's direct outbound is ready to
// accept dials, so any earlier attempt would hit "no available network
// interface" on Android.
type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger

	rtcOpt *UBClientcore.WebRTCOptions
	bfOpt  *UBClientcore.BroflakeOptions
	egOpt  *UBClientcore.EgressOptions
	tlsCfg *tlsProvider

	// Set at Start():
	broflakeConn *UBClientcore.BroflakeConn
	dial         UBClientcore.SOCKS5Dialer
	ui           UBClientcore.UI
	ql           *UBClientcore.QUICLayer
	uot          *uot.Client
}

// tlsProvider carries the inputs needed to generate a TLS config lazily at
// Start time, so a key-generation failure surfaces as a Start error rather
// than an option-parsing error.
type tlsProvider struct {
	insecure   bool
	ca         string
	serverName string
}

func NewOutbound(
	ctx context.Context,
	router adapter.Router,
	lg sblog.ContextLogger,
	tag string,
	opts option.UnboundedOutboundOptions,
) (adapter.Outbound, error) {
	bfOpt := UBClientcore.NewDefaultBroflakeOptions()
	setIfNonZero(&bfOpt.CTableSize, opts.CTableSize)
	setIfNonZero(&bfOpt.PTableSize, opts.PTableSize)
	setIfNonZero(&bfOpt.BusBufferSz, opts.BusBufferSz)
	setStringIfNonEmpty(&bfOpt.Netstated, opts.Netstated)

	rtcOpt := UBClientcore.NewDefaultWebRTCOptions()
	setStringIfNonEmpty(&rtcOpt.DiscoverySrv, opts.DiscoverySrv)
	setStringIfNonEmpty(&rtcOpt.Endpoint, opts.DiscoveryEndpoint)
	setStringIfNonEmpty(&rtcOpt.GenesisAddr, opts.GenesisAddr)
	setStringIfNonEmpty(&rtcOpt.Tag, opts.Tag)
	setStringIfNonEmpty(&rtcOpt.ConsumerSessionID, opts.ConsumerSessionID)
	setDurationSeconds(&rtcOpt.NATFailTimeout, opts.NATFailTimeout)
	setDurationSeconds(&rtcOpt.Patience, opts.Patience)
	setDurationSeconds(&rtcOpt.ErrorBackoff, opts.ErrorBackoff)
	if opts.STUNBatchSize != 0 {
		if opts.STUNBatchSize < 0 {
			return nil, fmt.Errorf("unbounded: stun_batch_size must be non-negative, got %d", opts.STUNBatchSize)
		}
		rtcOpt.STUNBatchSize = uint32(opts.STUNBatchSize)
	}
	rtcOpt.STUNBatch = randomSTUNBatch(opts.STUNServers)

	egOpt := UBClientcore.NewDefaultEgressOptions()
	setStringIfNonEmpty(&egOpt.Addr, opts.EgressAddr)
	setStringIfNonEmpty(&egOpt.Endpoint, opts.EgressEndpoint)
	setDurationSeconds(&egOpt.ConnectTimeout, opts.EgressConnectTimeout)
	setDurationSeconds(&egOpt.ErrorBackoff, opts.EgressErrorBackoff)

	// The sing-box dialer honours DialerOptions (detour, bind_interface, etc).
	// We use it for WebRTC candidate dials. Signaling uses a separate path
	// (the direct-transport injected via ctx) because it has to bypass the
	// VPN tunnel entirely.
	outboundDialer, err := dialer.New(ctx, opts.DialerOptions, opts.ServerIsDomain())
	if err != nil {
		return nil, fmt.Errorf("unbounded: build sing-box dialer: %w", err)
	}
	rtcNet, err := newRTCNet(ctx, outboundDialer, lg)
	if err != nil {
		return nil, fmt.Errorf("unbounded: build pion net shim: %w", err)
	}
	rtcOpt.Net = rtcNet
	rtcOpt.HTTPClient = signalingClient(ctx, outboundDialer)

	o := &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(
			C.TypeUnbounded,
			tag,
			[]string{N.NetworkTCP, N.NetworkUDP},
			opts.DialerOptions,
		),
		logger: lg,
		rtcOpt: rtcOpt,
		bfOpt:  bfOpt,
		egOpt:  egOpt,
		tlsCfg: &tlsProvider{
			insecure:   opts.InsecureDoNotVerifyClientCert,
			ca:         opts.EgressCA,
			serverName: opts.EgressServerName,
		},
	}
	o.uot = &uot.Client{
		Dialer:  (*uotDialer)(o),
		Version: uot.Version,
	}
	return o, nil
}

// Start brings broflake up and begins maintaining the QUIC connection to the
// paired peer. Runs at PostStart so the tunnel's direct outbound is ready.
func (o *Outbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStatePostStart {
		return nil
	}

	tlsConf, err := selfSignedTLSConfig(o.tlsCfg.insecure, o.tlsCfg.ca, o.tlsCfg.serverName)
	if err != nil {
		return fmt.Errorf("unbounded: build TLS config: %w", err)
	}

	bfConn, ui, err := UBClientcore.NewBroflake(o.bfOpt, o.rtcOpt, o.egOpt)
	if err != nil {
		return fmt.Errorf("unbounded: start broflake: %w", err)
	}
	ql, err := UBClientcore.NewQUICLayer(bfConn, tlsConf)
	if err != nil {
		ui.Stop()
		return fmt.Errorf("unbounded: start QUIC layer: %w", err)
	}

	o.broflakeConn = bfConn
	o.ui = ui
	o.ql = ql
	o.dial = UBClientcore.CreateSOCKS5Dialer(ql)

	go o.ql.ListenAndMaintainQUICConnection()
	return nil
}

func (o *Outbound) Close() error {
	if o.ql != nil {
		o.ql.Close()
	}
	if o.ui != nil {
		o.ui.Stop()
	}
	return nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	// Populate outbound / destination on the connection metadata so per-outbound
	// routing, logging, and tracker integrations see this dial. All the other
	// custom outbounds in lantern-box do this; Nelson's earlier draft only did
	// it on ListenPacket.
	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination

	if o.dial == nil {
		return nil, fmt.Errorf("unbounded: not started (DialContext called before Start)")
	}
	switch N.NetworkName(network) {
	case N.NetworkTCP:
		o.logger.DebugContext(ctx, "unbounded TCP dial to ", destination)
		return o.dial(ctx, network, destination.String())
	case N.NetworkUDP:
		o.logger.DebugContext(ctx, "unbounded UoT dial to ", destination)
		return o.uot.DialContext(ctx, network, destination)
	}
	return nil, fmt.Errorf("unbounded: unsupported network %q", network)
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination

	if o.dial == nil {
		return nil, fmt.Errorf("unbounded: not started (ListenPacket called before Start)")
	}
	o.logger.DebugContext(ctx, "unbounded UoT packet dial to ", destination)
	return o.uot.ListenPacket(ctx, destination)
}

// signalingClient returns the HTTP client broflake uses to reach freddie.
// Preference order:
//  1. A direct-transport RoundTripper registered on ctx by the tunnel process
//     (typically a kindling-backed transport that dials via sing-box's direct
//     outbound, applying socket protection automatically). This is the path
//     for production VPN operation.
//  2. Fall back to a plain transport that dials via the outbound's own dialer.
//     This does NOT bypass the tunnel, so it's only suitable for standalone
//     sing-box use and tests — not on-device production.
func signalingClient(ctx context.Context, fallback N.Dialer) *http.Client {
	if rt := lbAdapter.DirectTransportFromContext(ctx); rt != nil {
		return &http.Client{Transport: rt}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return fallback.DialContext(ctx, network, M.ParseSocksaddr(addr))
		},
	}
	return &http.Client{Transport: tr}
}

// randomSTUNBatch returns a batch-sampler closure over the given pool. Each
// call picks `size` servers uniformly at random from the pool so we don't
// expose a static list ordering on the wire.
//
// Returns an empty batch (no error) when the pool is empty — broflake tolerates
// that; nothing fingerprintable leaks.
func randomSTUNBatch(pool []string) func(size uint32) ([]string, error) {
	// Copy so callers mutating the options slice post-construction don't race.
	servers := make([]string, len(pool))
	copy(servers, pool)
	return func(size uint32) ([]string, error) {
		if len(servers) == 0 || size == 0 {
			return nil, nil
		}
		// Take min(size, len(servers)) random unique entries. Use crypto/rand
		// for the selection — a static pseudo-random stream would itself be
		// fingerprintable. The cost (a handful of calls) is irrelevant next to
		// STUN-server latency.
		n := int(size)
		if n > len(servers) {
			n = len(servers)
		}
		chosen := make([]string, 0, n)
		remaining := make([]string, len(servers))
		copy(remaining, servers)
		for i := 0; i < n; i++ {
			idx, err := cryptoRandIntN(len(remaining))
			if err != nil {
				return nil, fmt.Errorf("unbounded: pick STUN server: %w", err)
			}
			chosen = append(chosen, remaining[idx])
			remaining[idx] = remaining[len(remaining)-1]
			remaining = remaining[:len(remaining)-1]
		}
		return chosen, nil
	}
}

// cryptoRandIntN returns a uniform random int in [0, n). Delegates to
// crypto/rand.Int, which uses rejection sampling to avoid modulo bias —
// important for a small pool where a naive `% n` would skew toward the
// lower end of the range when n doesn't divide 2^64 evenly.
func cryptoRandIntN(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("n must be positive, got %d", n)
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// uotDialer adapts the broflake SOCKS5 dialer to N.Dialer so uot.Client can use
// it. We keep it as a type alias around *Outbound rather than a stored field
// so there's no cycle to watch.
type uotDialer Outbound

func (d *uotDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if d.dial == nil {
		return nil, fmt.Errorf("unbounded: not started")
	}
	return d.dial(ctx, network, destination.String())
}

func (d *uotDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	// uot.Client's Dialer only dials TCP for the UoT tunnel — the PacketConn
	// case here should be unreachable, but return a clear error if it isn't.
	return nil, fmt.Errorf("unbounded: uotDialer does not support ListenPacket: %w", os.ErrInvalid)
}

// Helpers — keep NewOutbound readable by pushing the zero-check logic out.

func setIfNonZero[T int](dst *T, v T) {
	if v != 0 {
		*dst = v
	}
}

func setStringIfNonEmpty(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func setDurationSeconds(dst *time.Duration, seconds int) {
	if seconds != 0 {
		*dst = time.Duration(seconds) * time.Second
	}
}
