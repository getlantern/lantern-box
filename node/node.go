// Package node runs lantern-box as a Lantern node whose config comes from
// lantern-cloud: it registers through the peer API Share My Connection uses,
// serves the config it gets back, proves to lantern-cloud that its inbound is
// reachable, keeps the route alive with heartbeats, and registers again on a
// fixed interval for fresh credentials and settings.
package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	C "github.com/getlantern/lantern-box/constant"
	LO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/unbounded"
	"github.com/getlantern/lantern-box/tracker/peerconn"
)

const (
	// DefaultRotationInterval matches Share My Connection's credential
	// rotation. Each rotation also picks up lantern-cloud's current settings,
	// such as the volunteer's discovery server and tuning, so it bounds how
	// stale they can get.
	DefaultRotationInterval = time.Hour

	// defaultHeartbeatInterval is used when lantern-cloud doesn't say.
	defaultHeartbeatInterval = 150 * time.Second
	verifyTimeout            = time.Minute
	requestTimeout           = 30 * time.Second
	minRetry                 = 15 * time.Second
	maxRetry                 = 5 * time.Minute
)

// Config configures a node.
type Config struct {
	// APIURL is lantern-cloud's API base URL, with its version segment.
	APIURL string
	// StateDir holds the node's device ID and port across restarts.
	StateDir string
	// Port, when set, replaces the port kept in StateDir, for an operator who
	// forwards a specific port.
	Port uint16
	// UserID is the operator's Lantern user ID, which the unbounded feature
	// flag can enroll.
	UserID string
	// FeatureOverrides is sent as X-Lantern-Feature-Override, for staging.
	FeatureOverrides string
	// MaxPeers and BandwidthLimit are the operator's limits for the
	// Unbounded volunteer service, applied to whatever lantern-cloud sends.
	MaxPeers       int
	BandwidthLimit string
	// RotationInterval is how often the node registers again. Zero means
	// DefaultRotationInterval.
	RotationInterval time.Duration
	// Version is reported to lantern-cloud.
	Version    string
	HTTPClient *http.Client
}

// boxService is a running box.
type boxService interface {
	Close() error
}

// boxFactory builds and starts a box from sing-box options JSON.
type boxFactory func(ctx context.Context, options string) (boxService, error)

// session is one registered route and the box serving it.
type session struct {
	routeID   string
	box       boxService
	heartbeat time.Duration
}

type node struct {
	cfg      Config
	api      *api
	port     uint16
	newBox   boxFactory
	clients  *clientCounter
	rotation time.Duration
	// heartbeatOverride replaces lantern-cloud's interval in tests.
	heartbeatOverride time.Duration
}

// Run serves as a node until ctx is done, then deregisters. ctx must carry
// lantern-box's option registries, as box.BaseContext does. It returns an
// error only when the node can't start at all, such as when its state can't
// be read; lantern-cloud and box failures are logged and retried.
func Run(ctx context.Context, cfg Config) error {
	if cfg.MaxPeers < 0 {
		return fmt.Errorf("max peers must not be negative, got %d", cfg.MaxPeers)
	}
	// Checked here because a box that fails to build can leave its inbound's
	// port bound, so every retry would then fail.
	if err := unbounded.ValidateBandwidthLimit(cfg.BandwidthLimit); err != nil {
		return fmt.Errorf("bandwidth limit: %w", err)
	}
	// Two nodes sharing a state directory would register the same route and
	// replace each other's credentials.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", cfg.StateDir, err)
	}
	lock := flock.New(filepath.Join(cfg.StateDir, "node.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("lock %s: %w", cfg.StateDir, err)
	}
	if !locked {
		return fmt.Errorf("another lantern-box serve is using %s", cfg.StateDir)
	}
	defer func() { _ = lock.Unlock() }()

	st, err := loadState(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load node state: %w", err)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	n := &node{
		cfg: cfg,
		api: &api{httpClient: httpClient, baseURL: cfg.APIURL, deviceID: st.DeviceID,
			userID: cfg.UserID, featureOverrides: cfg.FeatureOverrides, version: cfg.Version},
		port:     st.Port,
		newBox:   newSingBox,
		clients:  newClientCounter(),
		rotation: cfg.RotationInterval,
	}
	if cfg.Port != 0 {
		n.port = cfg.Port
	}
	if n.rotation <= 0 {
		n.rotation = DefaultRotationInterval
	}
	// The device ID authorizes deregistering the node's route, so only a
	// fingerprint of it is logged.
	sum := sha256.Sum256([]byte(st.DeviceID))
	log.Info("node: device ", hex.EncodeToString(sum[:4]), ", port ", n.port, ", state in ", cfg.StateDir)
	return n.run(ctx)
}

func (n *node) run(ctx context.Context) error {
	peerconn.SetListener(n.clients.listen)
	defer peerconn.SetListener(nil)

	retry := minRetry
	for {
		s, err := n.start(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error("node: ", err, "; retrying in ", retry)
			if !sleep(ctx, retry) {
				return nil
			}
			retry = min(retry*2, maxRetry)
			continue
		}
		retry = minRetry
		n.serve(ctx, s)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// start registers a route, starts a box with its config and has lantern-cloud
// verify it. On failure it leaves nothing running or registered.
func (n *node) start(ctx context.Context) (*session, error) {
	resp, err := n.register(ctx)
	if err != nil {
		return nil, err
	}
	s, err := n.launch(ctx, resp)
	if err != nil {
		n.deregister(resp.RouteID)
		return nil, err
	}
	return s, nil
}

func (n *node) register(ctx context.Context) (*registerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return n.api.register(ctx, registerRequest{
		ExternalPort:           n.port,
		InternalPort:           n.port,
		ServePool:              true,
		RunsUnboundedVolunteer: true,
	})
}

// launch starts a box for a registered route and verifies it. On failure the
// box is closed, but the route is the caller's to deregister.
func (n *node) launch(ctx context.Context, resp *registerResponse) (*session, error) {
	options, volunteer, err := applyOperatorLimits(ctx, resp.ServerConfig, n.cfg.MaxPeers, n.cfg.BandwidthLimit)
	if err != nil {
		return nil, err
	}
	if !volunteer {
		log.Warn("node: lantern-cloud sent no Unbounded volunteer service, so this node serves only its inbound")
	}
	b, err := n.newBox(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("start box: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	if err := n.api.verify(vctx, resp.RouteID); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("%w (is port %d reachable from the internet?)", err, n.port)
	}
	heartbeat := time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeatInterval
	}
	if n.heartbeatOverride > 0 {
		heartbeat = n.heartbeatOverride
	}
	log.Info("node: serving route ", resp.RouteID, " on port ", n.port)
	return &session{routeID: resp.RouteID, box: b, heartbeat: heartbeat}, nil
}

// serve heartbeats and rotates s until ctx is done or the route is lost. It
// returns with nothing running; on ctx done it has deregistered too.
func (n *node) serve(ctx context.Context, s *session) {
	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	rotate := time.NewTimer(n.rotation)
	defer rotate.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = s.box.Close()
			n.deregister(s.routeID)
			log.Info("node: stopped")
			return
		case <-heartbeat.C:
			hctx, cancel := context.WithTimeout(ctx, requestTimeout)
			err := n.api.heartbeat(hctx, s.routeID, n.clients.count())
			cancel()
			if isNotFound(err) {
				log.Warn("node: lantern-cloud dropped route ", s.routeID, ", registering again")
				_ = s.box.Close()
				return
			}
			if err != nil && ctx.Err() == nil {
				log.Warn("node: ", err)
			}
		case <-rotate.C:
			next, err := n.rotate(ctx, s)
			if err != nil {
				if ctx.Err() == nil {
					log.Error("node: rotate: ", err)
				}
				return
			}
			s = next
			heartbeat.Reset(s.heartbeat)
			rotate.Reset(n.rotation)
		}
	}
}

// rotate registers s's route again for fresh credentials and settings, and
// restarts the box with them. lantern-cloud keys routes by address and port,
// so the route ID normally stays the same; a different one means the old
// route is no longer this node's to serve, and it is deregistered.
//
// If lantern-cloud refuses the registration with a 4xx, s keeps serving. If
// the request failed some other way, including a 5xx a gateway can return
// after lantern-cloud committed, the route may now advertise credentials the
// box doesn't have, so nothing is kept. Otherwise the box has
// to close before the new one can take the port. On error nothing is left
// running or registered, and the caller starts over.
func (n *node) rotate(ctx context.Context, s *session) (*session, error) {
	resp, err := n.register(ctx)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status < http.StatusInternalServerError {
			log.Warn("node: rotate: ", err, "; keeping route ", s.routeID)
			return s, nil
		}
		_ = s.box.Close()
		n.deregister(s.routeID)
		return nil, err
	}
	_ = s.box.Close()
	if resp.RouteID != s.routeID {
		n.deregister(s.routeID)
	}
	next, err := n.launch(ctx, resp)
	if err != nil {
		n.deregister(resp.RouteID)
		return nil, err
	}
	return next, nil
}

// deregister drops a route, on its own context so it runs while the node
// shuts down. Failure is only logged: the route expires on its own.
func (n *node) deregister(routeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := n.api.deregister(ctx, routeID); err != nil {
		log.Warn("node: ", err)
	}
}

// applyOperatorLimits sets the operator's limits on the Unbounded volunteer
// service in options, and reports whether there is one. ctx carries the
// option registries.
func applyOperatorLimits(ctx context.Context, options string, maxPeers int, bandwidthLimit string) (string, bool, error) {
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(options))
	if err != nil {
		return "", false, fmt.Errorf("parse server config: %w", err)
	}
	found := false
	for _, svc := range opts.Services {
		vol, ok := svc.Options.(*LO.UnboundedVolunteerOptions)
		if svc.Type != C.TypeUnbounded || !ok {
			continue
		}
		found = true
		if maxPeers > 0 {
			vol.MaxPeers = maxPeers
		}
		if bandwidthLimit != "" {
			vol.BandwidthLimit = bandwidthLimit
		}
	}
	if !found {
		return options, false, nil
	}
	b, err := json.MarshalContext(ctx, opts)
	if err != nil {
		return "", false, fmt.Errorf("marshal server config: %w", err)
	}
	return string(b), true, nil
}

func newSingBox(ctx context.Context, options string) (boxService, error) {
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(options))
	if err != nil {
		return nil, fmt.Errorf("parse server config: %w", err)
	}
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	if err != nil {
		return nil, err
	}
	if err := b.Start(); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// clientCounter counts distinct client IPs with a connection open, from the
// inbounds' peerconn events.
type clientCounter struct {
	mu   sync.Mutex
	open map[string]int
}

func newClientCounter() *clientCounter {
	return &clientCounter{open: make(map[string]int)}
}

func (c *clientCounter) listen(evt peerconn.Event) {
	ip, _, err := net.SplitHostPort(evt.Source)
	if err != nil || ip == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open[ip] += evt.State
	if c.open[ip] <= 0 {
		delete(c.open, ip)
	}
}

func (c *clientCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.open)
}

// sleep waits d, and reports false if ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
