package unbounded

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/pion/ice/v4"
	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/common/dialer"
	sblog "github.com/sagernet/sing-box/log"
	sbOption "github.com/sagernet/sing-box/option"

	C "github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// RegisterService registers the Unbounded volunteer with the given sing-box ServiceRegistry. It
// shares the outbound's type name; the two live in separate registries.
func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.UnboundedVolunteerOptions](registry, C.TypeUnbounded, NewVolunteer)
}

const (
	defaultStatsInterval = time.Minute

	// natFailTimeoutKey is the tuning key lantern-cloud's unbounded_volunteer_tuning uses for the
	// volunteer's NAT fail timeout, as radiance's widget reads it.
	natFailTimeoutKey = "nat_fail_timeout_ms"
	minNATFailTimeout = time.Second
	maxNATFailTimeout = 30 * time.Second
)

// Volunteer runs broflake's volunteer role: it advertises the node to freddie, accepts WebRTC
// connections from the censored users freddie matches to it, and relays their traffic to the
// Unbounded egress.
type Volunteer struct {
	tag    string
	logger sblog.ContextLogger

	// ctx is the box's context, kept for Start: the signaling dialer resolves freddie's hostname
	// through the box's DNS, which is ready by then.
	ctx                   context.Context
	dialerOptions         sbOption.DialerOptions
	insecureDiscoveryCert bool

	bfOpt         *UBClientcore.BroflakeOptions
	rtcOpt        *UBClientcore.WebRTCOptions
	egOpt         *UBClientcore.EgressOptions
	statsInterval time.Duration
	bandwidth     byteRate
	// mappedPort is MappedUDPPort, whose socket Start opens and Close closes.
	mappedPort uint16

	stats volunteerStats

	mu     sync.Mutex
	ui     *UBClientcore.UIImpl
	mux    io.Closer
	cancel context.CancelFunc
	done   chan struct{}
}

func NewVolunteer(ctx context.Context, lg sblog.ContextLogger, tag string, opts option.UnboundedVolunteerOptions) (adapter.Service, error) {
	if opts.DiscoverySrv == "" {
		return nil, errors.New("unbounded volunteer: discovery_srv is required")
	}
	if opts.EgressAddr == "" {
		return nil, errors.New("unbounded volunteer: egress_addr is required")
	}
	if len(opts.STUNServers) == 0 {
		return nil, errors.New("unbounded volunteer: stun_servers is required")
	}
	for name, v := range map[string]int{
		"max_peers":              opts.MaxPeers,
		"bus_buffer_sz":          opts.BusBufferSz,
		"stun_batch_size":        opts.STUNBatchSize,
		"nat_fail_timeout":       opts.NATFailTimeout,
		"patience":               opts.Patience,
		"error_backoff":          opts.ErrorBackoff,
		"egress_connect_timeout": opts.EgressConnectTimeout,
		"egress_error_backoff":   opts.EgressErrorBackoff,
	} {
		// broflake sizes channels and timers from these, and a negative value panics or
		// fires at once.
		if v < 0 {
			return nil, fmt.Errorf("unbounded volunteer: %s must not be negative, got %d", name, v)
		}
	}

	if opts.MappedExternalIP != "" {
		if opts.MappedUDPPort == 0 {
			return nil, errors.New("unbounded volunteer: mapped_external_ip needs mapped_udp_port")
		}
		if ip := net.ParseIP(opts.MappedExternalIP); ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("unbounded volunteer: mapped_external_ip must be an IPv4 address, got %q", opts.MappedExternalIP)
		}
	}

	v := &Volunteer{
		tag:                   tag,
		logger:                lg,
		ctx:                   ctx,
		dialerOptions:         opts.DialerOptions,
		insecureDiscoveryCert: opts.InsecureDoNotVerifyDiscoveryCert,
	}

	bfOpt := UBClientcore.NewDefaultBroflakeOptions()
	bfOpt.ClientType = "widget"
	setIfNonZero(&bfOpt.CTableSize, opts.MaxPeers)
	// broflake's widget routing maps WebRTC slot i to egress connection i, so the tables must match.
	bfOpt.PTableSize = bfOpt.CTableSize
	setIfNonZero(&bfOpt.BusBufferSz, opts.BusBufferSz)
	setStringIfNonEmpty(&bfOpt.Netstated, opts.Netstated)
	bfOpt.OnConnectionChangeFunc = v.stats.connectionChange
	bfOpt.OnUpstreamChunkFunc = func(size, _ int) { v.stats.up.Add(uint64(size)) }
	bfOpt.OnDownstreamChunkFunc = func(size, _ int) { v.stats.down.Add(uint64(size)) }

	rtcOpt := UBClientcore.NewDefaultWebRTCOptions()
	rtcOpt.DiscoverySrv = opts.DiscoverySrv
	setStringIfNonEmpty(&rtcOpt.Endpoint, opts.DiscoveryEndpoint)
	setDurationSeconds(&rtcOpt.NATFailTimeout, opts.NATFailTimeout)
	setDurationSeconds(&rtcOpt.Patience, opts.Patience)
	setDurationSeconds(&rtcOpt.ErrorBackoff, opts.ErrorBackoff)
	natFail, tuning, ignored := volunteerTuning(opts.Tuning)
	rtcOpt.Tuning = tuning
	if natFail > 0 {
		rtcOpt.NATFailTimeout = natFail
	}
	if len(ignored) > 0 {
		lg.WarnContext(ctx, "unbounded volunteer: ignoring unknown or invalid tuning options", "ignored", strings.Join(ignored, ","))
	}
	if opts.STUNBatchSize > 0 {
		rtcOpt.STUNBatchSize = uint32(opts.STUNBatchSize)
	}
	rtcOpt.STUNBatch = randomSTUNBatch(opts.STUNServers)
	rtcOpt.TargetCountries = opts.TargetCountries
	// freddie counts outcomes for each kind of volunteer, so a lantern-box
	// node can be compared with the apps and the browser widget.
	rtcOpt.VolunteerKind = UBCommon.VolunteerKindServer
	rtcOpt.OnSTUNPreflight = v.stunPreflight
	v.mappedPort = opts.MappedUDPPort
	if opts.MappedExternalIP != "" {
		rtcOpt.ICEUDPMuxExternalIP = opts.MappedExternalIP
		rtcOpt.VolunteerMapped = true
	}

	bandwidth, err := parseRate(opts.BandwidthLimit)
	if err != nil {
		return nil, fmt.Errorf("unbounded volunteer: bandwidth_limit: %w", err)
	}
	if bandwidth > 0 {
		// broflake opens the volunteer's WebRTC sockets, and its STUN check's, on this Net.
		limited, err := newLimitedNet(bandwidth)
		if err != nil {
			return nil, fmt.Errorf("unbounded volunteer: bandwidth_limit: %w", err)
		}
		rtcOpt.Net = limited
	}
	v.bandwidth = bandwidth

	egOpt := UBClientcore.NewDefaultEgressOptions()
	egOpt.Addr = opts.EgressAddr
	setStringIfNonEmpty(&egOpt.Endpoint, opts.EgressEndpoint)
	setDurationSeconds(&egOpt.ConnectTimeout, opts.EgressConnectTimeout)
	setDurationSeconds(&egOpt.ErrorBackoff, opts.EgressErrorBackoff)

	v.bfOpt, v.rtcOpt, v.egOpt = bfOpt, rtcOpt, egOpt
	switch {
	case opts.StatsInterval == 0:
		v.statsInterval = defaultStatsInterval
	case opts.StatsInterval > 0:
		v.statsInterval = time.Duration(opts.StatsInterval) * time.Second
	}
	return v, nil
}

func (v *Volunteer) Type() string { return C.TypeUnbounded }
func (v *Volunteer) Tag() string  { return v.tag }

// Start runs broflake once the box is up. A volunteer only dials out, so it needs nothing from
// the box's inbounds or outbounds.
func (v *Volunteer) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ui != nil {
		return nil
	}
	// freddie is normally a hostname, which the dialer has to resolve through the box's DNS.
	signalingDialer, err := dialer.New(v.ctx, v.dialerOptions, true)
	if err != nil {
		return fmt.Errorf("unbounded volunteer: build signaling dialer: %w", err)
	}
	v.rtcOpt.HTTPClient = signalingClient(v.ctx, signalingDialer, v.insecureDiscoveryCert)
	if v.mappedPort != 0 {
		mux, err := v.listenMapped()
		if err != nil {
			return err
		}
		v.rtcOpt.ICEUDPMux, v.mux = mux, mux
	}
	_, ui, err := UBClientcore.NewBroflake(v.bfOpt, v.rtcOpt, v.egOpt)
	if err != nil {
		if v.mux != nil {
			_ = v.mux.Close()
			v.rtcOpt.ICEUDPMux, v.mux = nil, nil
		}
		return fmt.Errorf("unbounded volunteer: start broflake: %w", err)
	}
	v.ui = ui
	v.logger.InfoContext(context.Background(), "unbounded volunteer started",
		"discovery_srv", v.rtcOpt.DiscoverySrv,
		"egress_addr", v.egOpt.Addr,
		"max_peers", v.bfOpt.CTableSize,
		// sing-box's logger formats only strings, numbers, errors and Stringers, and panics on a slice.
		"target_countries", strings.Join(v.rtcOpt.TargetCountries, ","),
		"nat_fail_timeout", v.rtcOpt.NATFailTimeout,
		"bandwidth_limit", v.bandwidth.String(),
		"mapped_udp_port", int(v.mappedPort),
		"mapped_external_ip", v.rtcOpt.ICEUDPMuxExternalIP,
	)
	if v.statsInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		v.cancel, v.done = cancel, make(chan struct{})
		go v.logStats(ctx, v.statsInterval, v.done)
	}
	return nil
}

func (v *Volunteer) Close() error {
	v.mu.Lock()
	ui, mux, cancel, done := v.ui, v.mux, v.cancel, v.done
	v.ui, v.mux, v.cancel, v.done = nil, nil, nil, nil
	v.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if ui != nil {
		ui.Stop()
	}
	// After broflake, which only stops using the mux as its workers wind down, and before the
	// box returns, so a node's next box can take the port.
	if mux != nil {
		return mux.Close()
	}
	return nil
}

// listenMapped opens the mapped UDP port, on the bandwidth-limited Net when there is one so the
// mapped traffic is limited too, and builds the ICE mux over it. Probes are answered below the
// mux.
func (v *Volunteer) listenMapped() (*ice.UDPMuxDefault, error) {
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: int(v.mappedPort)}
	var conn net.PacketConn
	var err error
	if v.rtcOpt.Net != nil {
		conn, err = v.rtcOpt.Net.ListenUDP("udp4", laddr)
	} else {
		conn, err = net.ListenUDP("udp4", laddr)
	}
	if err != nil {
		return nil, fmt.Errorf("unbounded volunteer: listen on mapped UDP port %d: %w", v.mappedPort, err)
	}
	return ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: probeEchoConn{conn}, Net: v.rtcOpt.Net}), nil
}

// stunPreflight logs STUN checks that fail. A node that can't reach any STUN server stops taking
// offers until one answers (getlantern/unbounded#491), which an operator needs to hear about.
func (v *Volunteer) stunPreflight(res UBClientcore.STUNPreflightResult) {
	if res.Outcome == UBClientcore.STUNPreflightOK {
		return
	}
	v.logger.WarnContext(context.Background(), "unbounded volunteer: no STUN server answered, backing off",
		"outcome", res.Outcome, "servers", res.Servers)
}

func (v *Volunteer) logStats(ctx context.Context, every time.Duration, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := v.stats.snapshot()
			v.logger.InfoContext(ctx, "unbounded volunteer stats",
				"active_peers", s.active,
				"sessions", s.sessions,
				"bytes_up", s.up,
				"bytes_down", s.down,
			)
		}
	}
}

// volunteerStats counts what the node served. broflake calls its callbacks on worker goroutines,
// so every field is atomic.
type volunteerStats struct {
	active   atomic.Int64
	sessions atomic.Uint64
	up       atomic.Uint64
	down     atomic.Uint64
}

// connectionChange follows broflake's producer connection changes: state 1 when a consumer
// connects to a worker slot, -1 when it disconnects.
func (s *volunteerStats) connectionChange(state, _ int, _ net.IP) {
	switch state {
	case 1:
		s.active.Add(1)
		s.sessions.Add(1)
	case -1:
		s.active.Add(-1)
	}
}

type statsSnapshot struct {
	active             int64
	sessions, up, down uint64
}

func (s *volunteerStats) snapshot() statsSnapshot {
	return statsSnapshot{
		active:   s.active.Load(),
		sessions: s.sessions.Load(),
		up:       s.up.Load(),
		down:     s.down.Load(),
	}
}

// volunteerTuning splits nat_fail_timeout_ms from the pion tuning keys, which broflake parses
// with the same bounds radiance applies to the apps' volunteers.
func volunteerTuning(opts map[string]string) (natFail time.Duration, tuning UBClientcore.WebRTCTuning, ignored []string) {
	raw, ok := opts[natFailTimeoutKey]
	if ok {
		opts = maps.Clone(opts)
		delete(opts, natFailTimeoutKey)
	}
	tuning, ignored = UBClientcore.ParseWebRTCTuning(opts)
	if !ok {
		return 0, tuning, ignored
	}
	ms, err := strconv.ParseUint(raw, 10, 32)
	natFail = time.Duration(ms) * time.Millisecond
	if err != nil || natFail < minNATFailTimeout || natFail > maxNATFailTimeout {
		return 0, tuning, append(ignored, fmt.Sprintf("%s=%q", natFailTimeoutKey, raw))
	}
	return natFail, tuning, ignored
}
