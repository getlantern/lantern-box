package node

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"

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
	// Exit is the volunteer's exit. RunExit only sets its router; it never calls SetEnabled.
	Exit *exit.Exit
	// NewBox builds and starts the exit box from options JSON. nil: sing-box in ctx. It is
	// exported for tests, and for embedders that build boxes their own way.
	NewBox func(ctx context.Context, options string) (ExitBox, error)
	// RotationInterval overrides the interval lantern-cloud gives. 0: lantern-cloud's, or an hour.
	RotationInterval time.Duration
}

// ExitBox is a running exit box. It is exported for NewBox.
type ExitBox interface {
	Router() adapter.ConnectionRouterEx
	Close() error
}

// exitRouterSetter is the part of exit.Exit the registrar drives.
type exitRouterSetter interface {
	SetRouter(r adapter.ConnectionRouterEx, validUntil time.Time)
}

type exitRunner struct {
	cfg      ExitConfig
	api      *api
	exit     exitRouterSetter
	newBox   func(ctx context.Context, options string) (ExitBox, error)
	minRetry time.Duration
	maxRetry time.Duration

	// Only the loop's goroutine touches these.
	box        ExitBox
	validUntil time.Time
	rotation   time.Duration
}

// RunExit keeps a WebRTC-only registration and its exit box current until ctx ends. ctx must
// carry lantern-box's option registries, as box.BaseContext does. It returns an error only
// for a config it can't run with; lantern-cloud and box failures are logged and retried.
// Exit boxes don't share services with ctx: each gets its own clone of ctx's registry.
//
// RunExit never turns the exit on or off: the embedder drives cfg.Exit's SetEnabled, and
// should cancel ctx while the exit is off, so RunExit stops registering every rotation. Each
// rotation closes the previous box, which resets the streams still routed through it.
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

// loop registers and launches, then waits a rotation, or backs off after a failure. It
// returns once ctx ends, with the router cleared and the box closed.
func (r *exitRunner) loop(ctx context.Context) error {
	defer r.close()
	retry := r.minRetry
	for {
		var wait time.Duration
		if err := r.refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logExitFailure(err, retry)
			wait = retry
			retry = min(retry*2, r.maxRetry)
		} else {
			wait = r.rotation
			retry = r.minRetry
		}
		if !r.wait(ctx, wait) {
			return nil
		}
	}
}

func (r *exitRunner) refresh(ctx context.Context) error {
	resp, err := r.register(ctx)
	if err != nil {
		return err
	}
	return r.launch(ctx, resp)
}

func (r *exitRunner) register(ctx context.Context) (*webrtcRegisterResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return r.api.registerWebRTC(ctx)
}

// launch builds an exit box from resp's config and swaps the exit onto its router, then
// closes the previous box, so running sessions move to the new router for their next stream.
// On failure the current box is left as it is.
func (r *exitRunner) launch(ctx context.Context, resp *webrtcRegisterResponse) error {
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(resp.ServerConfig))
	if err != nil {
		return &exitStageError{stage: "parse the exit config", err: err}
	}
	if err := peerrules.ValidateAbuseRulesOptions(opts); err != nil {
		return &exitStageError{stage: "check the exit config", err: err, loggable: true}
	}
	// Its errors are fixed text, naming no rule.
	if err := peerrules.ValidateDirectExitRulesOptions(opts); err != nil {
		return &exitStageError{stage: "check the exit config", err: err, loggable: true}
	}
	// The exit box only routes the exit's traffic: no listeners, and nothing else lantern-cloud's
	// config carries, such as the debug listener of its peer configs.
	opts.Inbounds, opts.Endpoints, opts.Services = nil, nil, nil
	opts.Experimental, opts.NTP = nil, nil
	// Exit traffic leaves on the physical interface, never through the volunteer's own tunnel.
	opts.Route.AutoDetectInterface = true
	// sing-box's router logs every routed connection's destination, and this box carries
	// strangers' traffic.
	opts.Log = &option.LogOptions{Disabled: true}
	options, err := json.MarshalContext(ctx, opts)
	if err != nil {
		return &exitStageError{stage: "marshal the exit config", err: err}
	}
	b, err := r.newBox(ctx, string(options))
	if err != nil {
		return &exitStageError{stage: "start the exit box", err: err}
	}
	rotation := r.cfg.RotationInterval
	if rotation <= 0 {
		rotation = DefaultRotationInterval
		if s := resp.RotationIntervalSeconds; s > 0 {
			rotation = time.Duration(min(s, int64(maxExitRotation/time.Second))) * time.Second
		}
	}
	prev := r.box
	r.box, r.rotation, r.validUntil = b, rotation, time.Now().Add(2*rotation)
	r.exit.SetRouter(b.Router(), r.validUntil)
	if prev != nil {
		_ = prev.Close()
	}
	return nil
}

// wait waits d, expiring the current box if its registration runs out first, and reports
// false if ctx ends.
func (r *exitRunner) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	var expiry <-chan time.Time
	if r.box != nil {
		e := time.NewTimer(time.Until(r.validUntil))
		defer e.Stop()
		expiry = e.C
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-expiry:
			expiry = nil
			r.expire()
		case <-t.C:
			return true
		}
	}
}

// expire stops serving a registration lantern-cloud no longer vouches for.
func (r *exitRunner) expire() {
	log.Warn("direct exit: registration expired, no longer serving until lantern-cloud registers this device again")
	r.close()
}

// close clears the exit's router before closing the box under it.
func (r *exitRunner) close() {
	if r.box == nil {
		return
	}
	r.exit.SetRouter(nil, time.Time{})
	_ = r.box.Close()
	r.box, r.validUntil = nil, time.Time{}
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

// logExitFailure logs err without the device ID, a response body, the config or an address.
func logExitFailure(err error, retry time.Duration) {
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
	return singExitBox{b}, nil
}

type singExitBox struct{ b *sbox.Box }

func (s singExitBox) Router() adapter.ConnectionRouterEx { return s.b.Router() }
func (s singExitBox) Close() error                       { return s.b.Close() }
