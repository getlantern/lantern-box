package unbounded

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/common/dialer"
	sblog "github.com/sagernet/sing-box/log"
	sbOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"

	C "github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/internal/directexit"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// RegisterService registers the Unbounded volunteer with the given sing-box ServiceRegistry. It
// shares the outbound's type name; the two live in separate registries.
func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.UnboundedVolunteerOptions](registry, C.TypeUnbounded, NewVolunteer)
}

const (
	defaultStatsInterval = time.Minute

	// directExitLifetime is how long a direct exit stays servable in a box whose context
	// carries no deadline, such as `lantern-box run`'s: as long as the box.
	directExitLifetime = 100 * 365 * 24 * time.Hour

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
	directExit            bool

	bfOpt         *UBClientcore.BroflakeOptions
	rtcOpt        *UBClientcore.WebRTCOptions
	egOpt         *UBClientcore.EgressOptions
	statsInterval time.Duration
	bandwidth     byteRate

	stats volunteerStats

	mu     sync.Mutex
	ui     *UBClientcore.UIImpl
	exit   *exit.Exit
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

	v := &Volunteer{
		tag:                   tag,
		logger:                lg,
		ctx:                   ctx,
		dialerOptions:         opts.DialerOptions,
		insecureDiscoveryCert: opts.InsecureDoNotVerifyDiscoveryCert,
		directExit:            opts.DirectExit,
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
	e := v.startExit()
	if e != nil {
		v.rtcOpt.Exit = e.Options()
	}
	_, ui, err := UBClientcore.NewBroflake(v.bfOpt, v.rtcOpt, v.egOpt)
	if err != nil {
		if e != nil {
			e.Close()
			v.rtcOpt.Exit = nil
		}
		return fmt.Errorf("unbounded volunteer: start broflake: %w", err)
	}
	v.ui, v.exit = ui, e
	v.logger.InfoContext(context.Background(), "unbounded volunteer started",
		"discovery_srv", v.rtcOpt.DiscoverySrv,
		"egress_addr", v.egOpt.Addr,
		"max_peers", v.bfOpt.CTableSize,
		// sing-box's logger formats only strings, numbers, errors and Stringers, and panics on a slice.
		"target_countries", strings.Join(v.rtcOpt.TargetCountries, ","),
		"nat_fail_timeout", v.rtcOpt.NATFailTimeout,
		"bandwidth_limit", v.bandwidth.String(),
		"direct_exit", e != nil,
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
	ui, e, cancel, done := v.ui, v.exit, v.cancel, v.done
	v.ui, v.exit, v.cancel, v.done = nil, nil, nil, nil
	v.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if ui != nil {
		ui.Stop()
	}
	if e != nil {
		e.Close()
	}
	return nil
}

// startExit makes the volunteer's direct exit, served through its box's router, so the box's
// route rules apply to every stream and flow. It returns nil when the volunteer has no direct
// exit, or can't make one, in which case it serves as a relay only. The box must have been
// built with the context peerrules.CheckDirectExit returned for its options: those rules are
// all that checks strangers' traffic.
func (v *Volunteer) startExit() *exit.Exit {
	if !v.directExit {
		return nil
	}
	if !directexit.Checked(v.ctx) {
		v.logger.WarnContext(v.ctx, "unbounded volunteer: the box's rules weren't checked for a direct exit, so no direct exit")
		return nil
	}
	router := service.FromContext[adapter.Router](v.ctx)
	if router == nil {
		v.logger.WarnContext(v.ctx, "unbounded volunteer: no router in the box, so no direct exit")
		return nil
	}
	e, err := exit.New(v.ctx, v.logger)
	if err != nil {
		v.logger.WarnContext(v.ctx, "unbounded volunteer: no direct exit: ", err)
		return nil
	}
	// A node's box carries the end of the registration it was built from, and the exit stops
	// there. Without one, as for `lantern-box run`, the exit lasts as long as the box.
	validUntil, ok := directexit.ValidUntil(v.ctx)
	if !ok {
		validUntil = time.Now().Add(directExitLifetime)
	}
	e.SetEnabled(true)
	e.SetRouter(router, validUntil)
	return e
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
