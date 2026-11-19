package node

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/internal/directexit"
	"github.com/getlantern/lantern-box/peerrules"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// maxExitRotation bounds the rotation interval lantern-cloud can set.
const maxExitRotation = 24 * time.Hour

// ExitConfig configures RunExit.
type ExitConfig struct {
	// APIURL is lantern-cloud's API base URL, with its version segment.
	APIURL   string
	DeviceID string
	// HTTPClient makes the requests to lantern-cloud, and is required. It must dial around the
	// VPN's tunnel, as radiance's bypass client does: inside the VPN's own process, sockets
	// bypass the tunnel while the system resolver points into it, so a default client can't
	// resolve lantern-cloud, and registration fails whenever the VPN is on.
	HTTPClient *http.Client
	// Header, when set, is applied to every request after the defaults, so an app can send
	// its own admission headers in place of lantern-box's. The defaults include
	// X-Lantern-App: lantern-box, which lantern-cloud admits as a node, so an app must
	// override it here. It must not drop X-Lantern-Device-Id or Content-Type.
	//
	// An app must set what its own peer API sends (radiance's common.NewRequestWithHeaders),
	// X-Lantern-Config-Client-IP included: lantern-cloud geolocates the caller to admit it, and
	// prefers that header over the address the request came from.
	Header  func(http.Header)
	Version string
	// Exit is the volunteer's exit. RunExit only sets its routers, one per kind of session; it
	// never calls SetEnabled.
	Exit *exit.Exit
	// NewBox builds and starts the exit box from options JSON. nil: sing-box in ctx. It is
	// exported for tests, and for embedders that build boxes their own way. It must return a box
	// that has run every start stage, as sing-box's Box.Start does: its router's PostStart is
	// when the route rules take up their rule sets, and RunExit serves the box as soon as it
	// returns.
	NewBox func(ctx context.Context, options string) (ExitBox, error)
	// RotationInterval overrides the interval lantern-cloud gives. 0: lantern-cloud's, or an hour.
	RotationInterval time.Duration
	// NotBefore, when in the future, is when RunExit first calls lantern-cloud: the hold-off
	// OnHoldOff last reported, which the embedder keeps across a restart so that restarting
	// RunExit can't cut it short.
	NotBefore time.Time
	// OnHoldOff, when set, is called with the time before which RunExit won't call
	// lantern-cloud again each time lantern-cloud answers busy (429 or 503), and with the
	// zero time once a registration succeeds after that. The wait is RunExit's back-off, or
	// the response's Retry-After when longer. Other failures hold off only within a run.
	OnHoldOff func(until time.Time)
	// RoutingMark, on Linux, is the mark the exit box's sockets carry: the VPN tun's
	// auto_redirect output mark (sing-tun's default, 0x2024, unless the tun sets
	// auto_redirect_output_mark), as radiance's Linux tun runs auto_redirect. The exit box has
	// no tun to mark its own sockets, and without the mark the VPN's nftables OUTPUT rules
	// redirect exit traffic into the volunteer's tunnel. Marking needs CAP_NET_ADMIN, and
	// a mark is harmless while the VPN is off. 0: the auto_redirect output mark of the
	// NetworkManager in ctx, when ctx is the VPN box's, read at each rotation, so a tun that
	// starts after a rotation is missed until the next one. Ignored elsewhere than Linux.
	RoutingMark uint32
	// NetworkReset, when set, resets the current exit boxes' networks on each receive, closing
	// its connections and re-seeding its interface state, as radiance does for the VPN box when
	// the default interface keeps its name and index but changes address (macOS and iOS), which
	// the box's interface monitor doesn't report. RunExit reads it only between registrations,
	// so the embedder should send without blocking, on a channel with a buffer of one.
	NetworkReset <-chan struct{}
	// ServePool, read at each registration, is the embedder's setting for helping strangers
	// too (radiance's "serve_pool"), sent as the registration's serve_pool. nil: true. When it
	// changes, the embedder should send on Reregister, as radiance re-registers its peer route.
	ServePool func() bool
	// Reregister, when set, makes RunExit register again at once on each receive, unless
	// lantern-cloud has asked it to hold off, after which it registers anyway. Like
	// NetworkReset it is read only between registrations, so the embedder should send without
	// blocking, on a channel with a buffer of one.
	Reregister <-chan struct{}
}

// probeMark sets mark on a throwaway socket, failing as the exit box's dials would. A var so
// tests can stand in for a process with or without CAP_NET_ADMIN.
var probeMark = func(mark uint32) error {
	lc := net.ListenConfig{Control: control.RoutingMark(mark)}
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	return pc.Close()
}

// isLinux is a var so tests can mark the exit box's sockets on any OS.
var isLinux = runtime.GOOS == "linux"

// ExitBox is a running exit box. It is exported for NewBox. Its Router must also look up rule
// sets by tag, as sing-box's adapter.Router does (directexit.RuleSetLookup): RunExit serves a
// box only once its abuse rule sets have loaded, and refuses one whose router can't tell it.
type ExitBox interface {
	Router() adapter.ConnectionRouterEx
	// ResetNetwork resets the box's network, as sing-box's NetworkManager.ResetNetwork does.
	ResetNetwork()
	Close() error
}

// exitRouterSetter is the part of exit.Exit the registrar drives.
type exitRouterSetter interface {
	SetRouter(kind exit.Kind, r adapter.ConnectionRouterEx, validUntil time.Time, ready func() error)
}

type exitRunner struct {
	cfg      ExitConfig
	api      *api
	exit     exitRouterSetter
	newBox   func(ctx context.Context, options string) (ExitBox, error)
	minRetry time.Duration
	maxRetry time.Duration

	// Only the loop's goroutine touches these.
	boxes    [2]kindBox // by exit.Kind
	rotation time.Duration
	// registeredAt is when lantern-cloud last answered a registration.
	registeredAt time.Time
	// heldOff is whether OnHoldOff last reported a hold-off, and holdOffUntil its end, before
	// which Reregister waits.
	heldOff      bool
	holdOffUntil time.Time
}

// kindBox is the exit box serving one kind of session, the options it was built from, and when
// its registration runs out.
type kindBox struct {
	box        ExitBox
	options    string
	validUntil time.Time
}

// exitKinds are the kinds of session, in the order RunExit launches their boxes.
var exitKinds = []exit.Kind{exit.Pool, exit.Personal}

// RunExit keeps a WebRTC-only registration and its exit boxes current until ctx ends. ctx must
// carry lantern-box's option registries, as box.BaseContext does. It returns an error only
// for a config it can't run with; lantern-cloud and box failures are logged and retried.
// Exit boxes don't share services with ctx: each gets its own clone of ctx's registry.
//
// Each registration can carry two configs: the pool policy, when lantern-cloud lets this
// device serve strangers, and the personal policy, when it has helper pairings. Each gets a
// box of its own, whose router serves only that kind of session (see exit.Kind), and a kind
// whose config is absent is cleared. The kinds fail and expire apart: a config that fails its
// checks leaves its kind's current box serving until its own registration runs out.
//
// RunExit never turns the exit on or off: the embedder drives cfg.Exit's SetEnabled, and
// should cancel ctx while the exit is off, so RunExit stops registering every rotation. A
// rotation that brings a kind a changed config closes that kind's previous box, which resets
// the streams still routed through it; an unchanged config keeps the box, its registration
// extended.
//
// The embedder must not run RunExit where the device's country is unknown or censored, as
// radiance's wantPairingRoute (backend/peer_pairings.go) gates its pairings-only route: the
// country override (RADIANCE_COUNTRY) wins over the saved country, and none at all counts as
// not allowed. lantern-cloud refuses such a volunteer too, but its geolocation can fail open.
func RunExit(ctx context.Context, cfg ExitConfig) error {
	r, err := newExitRunner(cfg)
	if err != nil {
		return err
	}
	return r.loop(ctx)
}

func newExitRunner(cfg ExitConfig) (*exitRunner, error) {
	if cfg.Exit == nil {
		return nil, errors.New("direct exit: no exit")
	}
	if cfg.APIURL == "" {
		return nil, errors.New("direct exit: no API URL")
	}
	if cfg.DeviceID == "" {
		return nil, errors.New("direct exit: no device ID")
	}
	if cfg.HTTPClient == nil {
		return nil, errors.New("direct exit: no HTTP client")
	}
	newBox := cfg.NewBox
	if newBox == nil {
		newBox = newExitSingBox
	}
	return &exitRunner{
		cfg: cfg,
		api: &api{httpClient: sameOrigin(cfg.HTTPClient), baseURL: cfg.APIURL, deviceID: cfg.DeviceID,
			version: cfg.Version, header: cfg.Header},
		exit:     cfg.Exit,
		newBox:   newBox,
		minRetry: minRetry,
		maxRetry: maxRetry,
	}, nil
}

// loop registers and launches, then waits until a rotation after the registration, or backs off
// after a failure. It
// returns once ctx ends, with the router cleared and the box closed.
func (r *exitRunner) loop(ctx context.Context) error {
	defer r.close()
	r.holdOffUntil = r.cfg.NotBefore
	if wait := time.Until(r.cfg.NotBefore); wait > 0 && !r.wait(ctx, wait) {
		return nil
	}
	retry := r.minRetry
	for {
		var wait time.Duration
		registeredBefore := r.registeredAt
		if err := r.refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			wait = retry
			retry = min(retry*2, r.maxRetry)
			if after, ok := busy(err); ok {
				wait = max(wait, after)
				r.holdOff(time.Now().Add(wait))
			} else if r.registeredAt.After(registeredBefore) && r.serving() {
				// lantern-cloud answered, and a kind's config failed, but the boxes this
				// registration renewed are due again a rotation after it: a back-off longer
				// than that would let them expire for the failing kind's sake.
				wait = min(wait, max(time.Until(r.registeredAt.Add(r.rotation)), 0))
			}
			logExitFailure(err, wait)
		} else {
			// The next registration is due a rotation after this one, however long its boxes
			// took to start, so they're renewed before their two rotations run out.
			wait = max(time.Until(r.registeredAt.Add(r.rotation)), 0)
			retry = r.minRetry
		}
		if !r.wait(ctx, wait) {
			return nil
		}
	}
}

// serving reports whether any kind has a box.
func (r *exitRunner) serving() bool {
	for _, kb := range r.boxes {
		if kb.box != nil {
			return true
		}
	}
	return false
}

// busy reports whether err is lantern-cloud answering busy, and the Retry-After it gave.
func busy(err error) (time.Duration, bool) {
	var ae *apiError
	if !errors.As(err, &ae) || (ae.status != http.StatusTooManyRequests && ae.status != http.StatusServiceUnavailable) {
		return 0, false
	}
	return ae.retryAfter, true
}

// holdOff reports a hold-off to the embedder, or its end with the zero time.
func (r *exitRunner) holdOff(until time.Time) {
	r.heldOff, r.holdOffUntil = !until.IsZero(), until
	if r.cfg.OnHoldOff != nil {
		r.cfg.OnHoldOff(until)
	}
}

func (r *exitRunner) refresh(ctx context.Context) error {
	resp, err := r.register(ctx)
	if err != nil {
		return err
	}
	// The registration lasts from lantern-cloud's answer, however long its boxes take to start.
	registeredAt := time.Now()
	r.registeredAt = registeredAt
	// lantern-cloud answered, so a hold-off it asked for is over, whatever the configs hold.
	if r.heldOff {
		r.holdOff(time.Time{})
	}
	return r.launch(ctx, resp, registeredAt)
}

func (r *exitRunner) register(ctx context.Context) (*webrtcRegisterResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	servePool := r.cfg.ServePool == nil || r.cfg.ServePool()
	return r.api.registerWebRTC(ctx, servePool)
}

// launch builds an exit box from each config in resp and swaps the exit onto its router for
// that kind, then closes the kind's previous box, so its running sessions move to the new
// router for their next stream. A kind whose box was built from the same options keeps it, its
// registration extended, so its streams survive; this matters most while the other kind's
// config keeps failing, when RunExit re-registers at every retry. A kind with no config is
// cleared. A kind whose config fails keeps its current box, and launch returns that failure,
// so RunExit retries soon, except for a routing mark the process can no longer set, which
// withdraws the kind's box too. Each kind's registration lasts two rotations from
// registeredAt; a box that finishes starting after that is closed unserved.
func (r *exitRunner) launch(ctx context.Context, resp *webrtcRegisterResponse, registeredAt time.Time) error {
	rotation := r.cfg.RotationInterval
	if rotation <= 0 {
		rotation = DefaultRotationInterval
		if s := resp.RotationIntervalSeconds; s > 0 {
			rotation = time.Duration(min(s, int64(maxExitRotation/time.Second))) * time.Second
		}
	}
	r.rotation = rotation
	validUntil := registeredAt.Add(2 * rotation)
	configs := [2]string{exit.Pool: resp.ServerConfig, exit.Personal: resp.PersonalServerConfig}
	var errs []error
	for _, kind := range exitKinds {
		if configs[kind] == "" {
			r.closeKind(kind)
			continue
		}
		options, err := r.prepare(ctx, kind, configs[kind])
		if err != nil {
			if errors.Is(err, errMarkRefused) {
				// The running box's sockets would go unmarked too, for auto_redirect to capture.
				r.closeKind(kind)
			}
			errs = append(errs, err)
			continue
		}
		kb := &r.boxes[kind]
		if kb.box != nil && kb.options == options {
			// A renewal rechecks the sets: an update may have emptied them since. The exit
			// already refuses the box's streams while they're empty; this keeps such a box
			// from being extended.
			if err := abuseRuleSetsLoaded(kb.box.Router()); err != nil {
				errs = append(errs, &exitStageError{stage: "check the " + kind.String() + " exit box's abuse rule sets", err: err, loggable: true})
				continue
			}
			kb.validUntil = validUntil
			r.exit.SetRouter(kind, kb.box.Router(), validUntil, readyCheck(kb.box))
			continue
		}
		b, err := r.newBox(ctx, options)
		if err != nil {
			errs = append(errs, &exitStageError{stage: "start the " + kind.String() + " exit box", err: err})
			continue
		}
		// sing-box starts with a remote rule set it couldn't download empty, so the abuse
		// rejects would match nothing ahead of the exit's final route. Whatever built the box,
		// RunExit serves it only once those sets have loaded, retrying on its back-off while the
		// kind's current box, if any, serves on.
		if err := abuseRuleSetsLoaded(b.Router()); err != nil {
			_ = b.Close()
			errs = append(errs, &exitStageError{stage: "check the " + kind.String() + " exit box's abuse rule sets", err: err, loggable: true})
			continue
		}
		if !time.Now().Before(validUntil) {
			_ = b.Close()
			errs = append(errs, &exitStageError{stage: "start the " + kind.String() + " exit box", err: errStartedTooLate, loggable: true})
			continue
		}
		prev := kb.box
		*kb = kindBox{box: b, options: options, validUntil: validUntil}
		r.exit.SetRouter(kind, b.Router(), validUntil, readyCheck(b))
		if prev != nil {
			_ = prev.Close()
		}
	}
	return errors.Join(errs...)
}

// readyCheck is the exit's ready check for box b: its abuse rule sets hold rules. The exit runs
// it at each stream and session, since a later download or reload can empty a set.
func readyCheck(b ExitBox) func() error {
	router := b.Router()
	return func() error { return abuseRuleSetsLoaded(router) }
}

// errRuleSetsUncheckable is a box whose router has no rule set lookup, so RunExit can't tell
// whether its abuse rule sets loaded.
var errRuleSetsUncheckable = errors.New("its router can't check its abuse rule sets (it has no RuleSet lookup)")

// abuseRuleSetsLoaded checks the abuse rule sets of a box's router, which must be able to look
// them up, as sing-box's adapter.Router can. Its errors name only tags.
func abuseRuleSetsLoaded(router adapter.ConnectionRouterEx) error {
	lookup, ok := router.(directexit.RuleSetLookup)
	if !ok {
		return errRuleSetsUncheckable
	}
	return directexit.AbuseRuleSetsLoaded(lookup)
}

// errStartedTooLate is a box that finished starting after its registration ran out.
var errStartedTooLate = errors.New("it started after its registration ran out")

// errMarkRefused is a routing mark the process may not set.
var errMarkRefused = errors.New("could not set the routing mark on a socket (it needs CAP_NET_ADMIN)")

// prepare checks and sanitizes a kind's config, returning the options for its box.
func (r *exitRunner) prepare(ctx context.Context, kind exit.Kind, config string) (string, error) {
	stage := func(what string) string { return what + " the " + kind.String() + " exit config" }
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(config))
	if err != nil {
		return "", &exitStageError{stage: stage("parse"), err: err}
	}
	// The personal policy has no allowlist, but the same abuse rejects as the pool's.
	if err := peerrules.ValidateAbuseRulesOptions(opts); err != nil {
		return "", &exitStageError{stage: stage("check"), err: err, loggable: true}
	}
	// Its errors are fixed text, naming no rule.
	if err := peerrules.ValidateDirectExitRulesOptions(opts); err != nil {
		return "", &exitStageError{stage: stage("check"), err: err, loggable: true}
	}
	// The exit box only routes the exit's traffic: no listeners, and nothing else lantern-cloud's
	// config carries, such as the debug listener of its peer configs.
	opts.Inbounds, opts.Endpoints, opts.Services = nil, nil, nil
	opts.Experimental, opts.NTP = nil, nil
	// Exit traffic leaves on the physical interface, never through the volunteer's own tunnel.
	opts.Route.AutoDetectInterface = true
	if mark := r.routingMark(ctx); mark != 0 {
		// A box whose every dial fails to set its mark would be offered while it serves
		// nothing, so a mark the process can't set serves no box of this kind.
		if probeMark(mark) != nil {
			return "", &exitStageError{stage: "mark the " + kind.String() + " exit box's sockets", err: errMarkRefused, loggable: true}
		}
		opts.Route.DefaultMark = option.FwMark(mark)
	}
	// sing-box's router logs every routed connection's destination, and this box carries
	// strangers' and friends' traffic.
	opts.Log = &option.LogOptions{Disabled: true}
	options, err := json.MarshalContext(ctx, opts)
	if err != nil {
		return "", &exitStageError{stage: stage("marshal"), err: err}
	}
	return string(options), nil
}

// routingMark is the mark for the exit box's sockets on Linux (see ExitConfig.RoutingMark), or 0.
func (r *exitRunner) routingMark(ctx context.Context) uint32 {
	if !isLinux {
		return 0
	}
	if r.cfg.RoutingMark != 0 {
		return r.cfg.RoutingMark
	}
	if nm := service.FromContext[adapter.NetworkManager](ctx); nm != nil {
		return nm.AutoRedirectOutputMark()
	}
	return 0
}

// wait waits d, expiring each kind's box if its registration runs out first and resetting the
// boxes' networks on each NetworkReset, and reports false if ctx ends. A Reregister ends the
// wait early, unless lantern-cloud has asked to hold off.
func (r *exitRunner) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	var e *time.Timer
	defer func() {
		if e != nil {
			e.Stop()
		}
	}()
	for {
		if e != nil {
			e.Stop()
			e = nil
		}
		var expiry <-chan time.Time
		if next, ok := r.nextExpiry(); ok {
			e = time.NewTimer(time.Until(next))
			expiry = e.C
		}
		select {
		case <-ctx.Done():
			return false
		case <-expiry:
			r.expireDue()
		case <-r.cfg.NetworkReset:
			for _, kb := range r.boxes {
				if kb.box != nil {
					kb.box.ResetNetwork()
				}
			}
		case <-r.cfg.Reregister:
			if !time.Now().Before(r.holdOffUntil) {
				return true
			}
		case <-t.C:
			return true
		}
	}
}

// nextExpiry is the earliest registration end of the boxes running, if any.
func (r *exitRunner) nextExpiry() (time.Time, bool) {
	var next time.Time
	for _, kb := range r.boxes {
		if kb.box != nil && (next.IsZero() || kb.validUntil.Before(next)) {
			next = kb.validUntil
		}
	}
	return next, !next.IsZero()
}

// expireDue stops serving each kind whose registration lantern-cloud no longer vouches for.
func (r *exitRunner) expireDue() {
	for _, kind := range exitKinds {
		if kb := r.boxes[kind]; kb.box != nil && !time.Now().Before(kb.validUntil) {
			log.Warn("direct exit: ", kind, " registration expired, no longer serving it until lantern-cloud registers this device again")
			r.closeKind(kind)
		}
	}
}

// close clears both kinds.
func (r *exitRunner) close() {
	for _, kind := range exitKinds {
		r.closeKind(kind)
	}
}

// closeKind clears kind's router on the exit before closing the box under it.
func (r *exitRunner) closeKind(kind exit.Kind) {
	kb := &r.boxes[kind]
	if kb.box == nil {
		return
	}
	r.exit.SetRouter(kind, nil, time.Time{}, nil)
	_ = kb.box.Close()
	*kb = kindBox{}
}

// exitStageError is a launch step that failed. Only a loggable error is logged whole: the
// others can quote the config, which names hosts.
type exitStageError struct {
	stage    string
	err      error
	loggable bool
}

func (e *exitStageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *exitStageError) Unwrap() error { return e.err }

// logExitFailure logs err without the device ID, a response body, the config or an address. A
// launch's failures, one per kind, are each logged.
func logExitFailure(err error, retry time.Duration) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, err := range joined.Unwrap() {
			logExitFailure(err, retry)
		}
		return
	}
	var ae *apiError
	var se *exitStageError
	switch {
	case errors.As(err, &ae) && ae.status == http.StatusForbidden:
		log.Warn("direct exit: lantern-cloud refused this device as a direct exit; retrying in ", retry)
	case errors.As(err, &ae):
		log.Warn("direct exit: register: lantern-cloud answered status ", ae.status, "; retrying in ", retry)
	case errors.As(err, &se) && se.loggable:
		log.Error("direct exit: ", se, "; retrying in ", retry)
	case errors.As(err, &se):
		log.Error("direct exit: could not ", se.stage, "; retrying in ", retry)
	case errors.Is(err, context.DeadlineExceeded):
		log.Warn("direct exit: register: lantern-cloud timed out; retrying in ", retry)
	case errors.Is(err, errNoExitConfig):
		log.Warn("direct exit: ", err, "; retrying in ", retry)
	case errors.As(err, new(*url.Error)):
		// Transport errors can name lantern-cloud's address, and decode errors its answer.
		log.Warn("direct exit: register: could not reach lantern-cloud; retrying in ", retry)
	default:
		log.Warn("direct exit: register: could not read lantern-cloud's answer; retrying in ", retry)
	}
}

// newExitSingBox is the default NewBox: sing-box in a clone of ctx's service registry, so it
// keeps ctx's option registries and cancellation but registers its managers only in its own
// registry, never over those of ctx's box or the previous exit box. The clone gets a log
// factory of its own that logs nothing: sing-box uses one it finds in its context over
// log.disabled, so it would log strangers' destinations through the caller's, and close it
// with the box.
func newExitSingBox(ctx context.Context, options string) (ExitBox, error) {
	ctx = service.ExtendContext(ctx)
	service.MustRegister[log.Factory](ctx, log.NewNOPFactory())
	b, err := startSingBox(ctx, options)
	if err != nil {
		return nil, err
	}
	return singExitBox{b: b, ctx: ctx}, nil
}

type singExitBox struct {
	b   *sbox.Box
	ctx context.Context
}

func (s singExitBox) Router() adapter.ConnectionRouterEx { return s.b.Router() }
func (s singExitBox) ResetNetwork()                      { s.b.Network().ResetNetwork(s.ctx) }
func (s singExitBox) Close() error                       { return s.b.Close() }
