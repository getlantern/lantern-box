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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	C "github.com/getlantern/lantern-box/constant"
	LO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/portforward"
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
	// portMappingTimeout bounds finding the router and asking it to map the
	// node's port, so a node with no router to ask, such as a VPS, starts
	// without much delay.
	portMappingTimeout = 30 * time.Second
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
	// SkipPortMapping stops the node asking its router, through UPnP, NAT-PMP
	// or PCP, to forward its TCP port, for an operator who forwards it by hand
	// or has nothing to forward.
	SkipPortMapping bool
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

// portMapper is the part of portforward.Forwarder a node uses.
type portMapper interface {
	MapPort(ctx context.Context, protocol string, internalPort uint16, description string) (*portforward.Mapping, error)
	StartRenewal(ctx context.Context)
	UnmapPort(ctx context.Context) error
}

type node struct {
	cfg  Config
	api  *api
	port uint16
	// externalPort is the port clients reach the node at: the one the router
	// maps to port, which can differ from it, or port itself without a
	// mapping. Zero means port.
	externalPort uint16
	// newMapper finds the router to map the port on. Nil maps nothing.
	newMapper func(ctx context.Context) (portMapper, error)
	newBox    boxFactory
	clients   *clientCounter
	rotation  time.Duration
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
		// No client-wide timeout: each request has its own deadline, and
		// verify, which waits on lantern-cloud dialing back, needs longer than
		// the rest.
		httpClient = &http.Client{}
	}
	n := &node{
		cfg: cfg,
		api: &api{httpClient: sameOrigin(httpClient), baseURL: cfg.APIURL, deviceID: st.DeviceID,
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
	n.newMapper = mapperFor(cfg)
	// The device ID authorizes deregistering the node's route, so only a
	// fingerprint of it is logged.
	sum := sha256.Sum256([]byte(st.DeviceID))
	log.Info("node: device ", hex.EncodeToString(sum[:4]), ", port ", n.port, ", state in ", cfg.StateDir)
	return n.run(ctx)
}

func (n *node) run(ctx context.Context) error {
	peerconn.SetListener(n.clients.listen)
	defer peerconn.SetListener(nil)
	// Before asking the router to forward the port, which would otherwise
	// open whatever else listens on it to the internet.
	if err := portFree(n.port); err != nil {
		return err
	}
	if m := n.mapPort(ctx); m != nil {
		defer func() {
			uctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			if err := m.UnmapPort(uctx); err != nil {
				log.Warn("node: remove router mapping: ", err)
			}
		}()
	}

	retry := minRetry
	for {
		s, err := n.start(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Another program has taken the port, and retrying would keep
			// the router forwarding to it. Stopping removes the mapping.
			if errors.Is(err, errPortInUse) {
				return err
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

// mapPort asks the node's router to forward a TCP port to its inbound, so
// clients can reach a node at home, and keeps the mapping renewed. It returns
// nil, and the node is reached on its own port, when there's no router to ask
// or it refuses: a node on a public address, or one whose operator forwards
// the port by hand, needs no mapping.
func (n *node) mapPort(ctx context.Context) portMapper {
	if n.newMapper == nil {
		return nil
	}
	mctx, cancel := context.WithTimeout(ctx, portMappingTimeout)
	defer cancel()
	m, err := n.newMapper(mctx)
	if err != nil {
		log.Info("node: no router to map port ", n.port, " on (", err, "), so clients must reach it directly")
		return nil
	}
	mapping, err := m.MapPort(mctx, portforward.TCP, n.port, "Lantern node")
	if err != nil {
		log.Warn("node: the router didn't map port ", n.port, " (", err, "), so clients must reach it directly")
		return nil
	}
	n.externalPort = mapping.ExternalPort
	m.StartRenewal(ctx)
	log.Info("node: the router maps TCP port ", mapping.ExternalPort, " to port ", n.port, " (", mapping.Method, ")")
	return m
}

// errPortInUse is a node's port already taken by another program.
var errPortInUse = errors.New("the node's port is in use by another program; choose another with --port")

// portFree reports errPortInUse if something already listens on port.
func portFree(port uint16) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(int(port))))
	if err != nil {
		if addrInUse(err) {
			return fmt.Errorf("port %d: %w", port, errPortInUse)
		}
		return fmt.Errorf("check port %d: %w", port, err)
	}
	return ln.Close()
}

func addrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || strings.Contains(err.Error(), "address already in use")
}

// clientPort is the port clients reach the node at.
func (n *node) clientPort() uint16 {
	if n.externalPort != 0 {
		return n.externalPort
	}
	return n.port
}

// mapperFor is how a node with cfg finds its router, or nil to map nothing.
func mapperFor(cfg Config) func(context.Context) (portMapper, error) {
	if cfg.SkipPortMapping {
		return nil
	}
	return newPortForwarder
}

// newPortForwarder returns a nil interface, not a nil *Forwarder, when there's
// no router to ask.
func newPortForwarder(ctx context.Context) (portMapper, error) {
	f, err := portforward.NewForwarder(ctx)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (n *node) register(ctx context.Context) (*registerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return n.api.register(ctx, registerRequest{
		ExternalPort:           n.clientPort(),
		InternalPort:           n.port,
		ServePool:              true,
		RunsUnboundedVolunteer: true,
	})
}

// launch starts a box for a registered route and verifies it. On failure the
// box is closed, but the route is the caller's to deregister.
func (n *node) launch(ctx context.Context, resp *registerResponse) (*session, error) {
	if resp.ServerConfig == "" {
		return nil, errors.New("register: response has no config")
	}
	options, volunteer, err := applyOperatorLimits(ctx, resp.ServerConfig, n.cfg.MaxPeers, n.cfg.BandwidthLimit)
	if err != nil {
		return nil, err
	}
	if !volunteer {
		log.Warn("node: lantern-cloud sent no Unbounded volunteer service, so this node serves only its inbound")
	}
	b, err := n.newBox(ctx, options)
	if err != nil {
		if addrInUse(err) {
			err = fmt.Errorf("%w: %w", errPortInUse, err)
		}
		return nil, fmt.Errorf("start box: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	if err := n.api.verify(vctx, resp.RouteID); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("%w (is TCP port %d reachable from the internet?)", err, n.clientPort())
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
			// The heartbeat schedule carries over to the new session unless
			// its interval changed: rotations closer together than heartbeats
			// would otherwise starve them, and lantern-cloud would never hear
			// the node's client count.
			if next.heartbeat != s.heartbeat {
				heartbeat.Reset(next.heartbeat)
			}
			s = next
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
