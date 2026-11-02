// Package portforward opens TCP and UDP ports on the local network gateway so
// a node's inbounds are reachable from the public internet without manual
// router configuration. It tries PCP, then NAT-PMP, then UPnP IGDv2, then
// IGDv1.
package portforward

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	semconv "github.com/getlantern/semconv"
	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
)

// ErrNoPortForwarding is returned when no gateway is reachable, the
// gateway refuses to map a port, or the discovery scan times out. Callers
// should treat this as "this network can't host a peer proxy" and surface it
// to the user rather than retry indefinitely.
var ErrNoPortForwarding = errors.New("no port forwarding available")

// Protocols a mapping can carry, as UPnP IGD names them.
const (
	TCP = "TCP"
	UDP = "UDP"
)

type Mapping struct {
	ExternalPort  uint16
	InternalPort  uint16
	InternalIP    string
	Protocol      string
	LeaseDuration time.Duration
	Method        string
	// description is what MapPort was given, shown in the router's UI. Renewals
	// reuse it.
	description string
	// requested is the lease MapPort asked for, in seconds, which renewals ask
	// for again. LeaseDuration is what the gateway granted, when it says.
	requested uint32
	// renewed is when the mapping was added or last confirmed renewed, and
	// retryAt, if set, when a failed renewal is retried. Guarded by the
	// Forwarder's mu.
	renewed time.Time
	retryAt time.Time
}

// leaseReporter is a client whose gateway says what lease it granted, as
// NAT-PMP and PCP gateways do. A grant can be shorter than the request, and
// renewal has to come before it runs out. UPnP IGD has no such answer.
type leaseReporter interface {
	lease(protocol string, internalPort uint16) (time.Duration, bool)
}

// minRenewInterval floors the renewal period a granted lease sets. Clients
// refuse fresh grants far shorter than this would need; it only guards against
// a renewal granting next to nothing.
const minRenewInterval = time.Second

// igdClient is the subset of the IGDv2/v1 clients we use. goupnp's generated
// clients already satisfy this shape.
type igdClient interface {
	AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddress() (string, error)
}

// Forwarder manages port mappings on the local gateway, at most one per
// protocol. Construct one per session.
type Forwarder struct {
	mu     sync.Mutex
	client igdClient
	method string
	// mappings holds the active mapping for each protocol.
	mappings map[string]*Mapping
	cancel   context.CancelFunc
	// teardowns counts UnmapPort calls. A renewal compares it before and
	// after its AddPortMapping, so one that lands after a teardown's delete
	// gets undone rather than left on the gateway forever. A count rather
	// than a flag, so a Forwarder that maps again after UnmapPort renews its
	// new mappings.
	teardowns uint64
	// wake tells renewLoop a mapping was added, so it reschedules for it.
	wake chan struct{}
	// renewWait, if set, replaces renewCallTimeout, for tests.
	renewWait time.Duration
	// inflight counts the adds on each port that haven't resolved and been
	// cleaned up after (see addLocked). MapPort refuses a port with any, so a
	// stale add's cleanup can't remove a new mapping of the same port.
	inflight map[portKey]int
}

type portKey struct {
	protocol string
	port     uint16
}

// ProbeUPnP reports whether discovery on the local network turns up a gateway
// that could host a port mapping, by PCP, NAT-PMP or UPnP IGD. No port is actually mapped — this
// is the "would Share My Connection's residential-proxy mode work on this
// network?" pre-flight check that UI flows show before committing to the
// long-lived SmC path.
//
// True means discovery succeeded; false means either no gateway was found
// (ErrNoPortForwarding) or ctx expired before discovery completed. Callers
// generally treat both as "not available" — distinguishing them requires
// returning the underlying error, which most UI surfaces don't have a
// productive use for.
//
// Pick a 5-10s timeout on ctx: M-SEARCH multicast waits for replies and a
// fast bail leaves slower gateways unmatched. The discovered forwarder is
// discarded after the probe; it holds no goroutines or sockets that need
// explicit cleanup, so a subsequent NewForwarder call can re-discover
// without coordination.
func ProbeUPnP(ctx context.Context) bool {
	_, err := NewForwarder(ctx)
	if err != nil {
		// NewForwarder logs the specifics; these lines exist so the probe's own
		// verdict is greppable. A bare bool at the call site cannot say whether
		// the answer was "no gateway" or "we ran out of time", and the UI that
		// consumes it shows the same thing either way.
		//
		// The two are kept apart deliberately. The docstring above promises that
		// false covers both, and calling a timeout "this network cannot host a
		// peer" states a capability verdict the probe did not reach — which is
		// the kind of thing an operator acts on once and does not revisit.
		if ctx.Err() != nil {
			slog.Info("portforward: UPnP probe was canceled or timed out; capability unknown", "err", err)
		} else {
			slog.Info("portforward: UPnP probe found no gateway; this network cannot host a peer", "err", err)
		}
		return false
	}
	slog.Info("portforward: UPnP probe succeeded")
	return true
}

// NewForwarder discovers the local gateway and returns a Forwarder bound to
// it, using the first of PCP, NAT-PMP, UPnP IGDv2 and IGDv1 the gateway
// speaks. Callers should pick a 5-10s timeout on ctx — UPnP discovery is
// M-SEARCH multicast and waits for replies. PCP and NAT-PMP take at most
// pmpProbeTimeout of it.
//
// Returns ErrNoPortForwarding only when discovery completes without finding
// a usable gateway. If ctx was canceled or its deadline expired during
// discovery, the ctx error is returned verbatim so callers can distinguish
// "this network can't host a peer" from "we ran out of time, retry later".
func NewForwarder(ctx context.Context) (*Forwarder, error) {
	// Both discovery errors were previously discarded by the `err == nil &&
	// c != nil` guards, which made a failure indistinguishable from "this
	// network has no gateway" — and the success path logs the method it chose
	// while the failure path logged nothing at all. Whether IGDv2 was refused
	// or simply absent is the first thing anyone asks when a peer turns out to
	// be unreachable.
	cp, method, errPMP := discoverPMP(ctx, nil)
	if errPMP == nil {
		slog.Info("portforward: gateway discovered", "method", method)
		return &Forwarder{client: cp, method: method}, nil
	}
	c2, err2 := discoverIGDv2(ctx)
	if err2 == nil && c2 != nil {
		slog.Info("portforward: gateway discovered", "method", "upnp-igd2", "pmp_err", errPMP)
		return &Forwarder{client: c2, method: "upnp-igd2"}, nil
	}
	c1, err1 := discoverIGDv1(ctx)
	if err1 == nil && c1 != nil {
		slog.Info("portforward: gateway discovered", "method", "upnp-igd1",
			// Worth knowing: a router that answers v1 but not v2 is common, and
			// it is also the shape of a gateway with a partial UPnP stack.
			"igd2_err", errOrAbsent(err2, c2), "pmp_err", errPMP)
		return &Forwarder{client: c1, method: "upnp-igd1"}, nil
	}

	if err := ctx.Err(); err != nil {
		slog.Warn("portforward: gateway discovery was canceled or timed out",
			"err", err, "pmp_err", errPMP, "igd2_err", errOrAbsent(err2, c2), "igd1_err", errOrAbsent(err1, c1))
		return nil, err
	}
	slog.Warn("portforward: no gateway found; this network cannot host a peer",
		"pmp_err", errPMP, "igd2_err", errOrAbsent(err2, c2), "igd1_err", errOrAbsent(err1, c1))
	return nil, ErrNoPortForwarding
}

// errOrAbsent renders a discovery outcome for a log field. A nil error with a
// nil client is not a failure — it means discovery completed and the network
// simply has no gateway of that flavour — and reporting that as "<nil>" reads
// as though nothing was attempted.
func errOrAbsent(err error, c igdClient) string {
	if err != nil {
		return err.Error()
	}
	if c == nil {
		return "no gateway responded"
	}
	return "ok"
}

// MapPort asks the gateway to forward externalPort → (LocalIP():internalPort)
// for protocol, TCP or UDP. A Forwarder holds one mapping per protocol. Lease
// duration is requested as 1 hour but some routers ignore the request and
// assign their own (or none — "permanent"). description is shown in the
// router's UI so users can identify and remove the mapping manually if needed.
func (f *Forwarder) MapPort(ctx context.Context, protocol string, internalPort uint16, description string) (*Mapping, error) {
	if protocol != TCP && protocol != UDP {
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mappings[protocol] != nil {
		return nil, fmt.Errorf("forwarder already has an active %s mapping", protocol)
	}
	if f.inflight[portKey{protocol, internalPort}] > 0 {
		return nil, fmt.Errorf("%s port %d is still being cleaned up after an earlier attempt", protocol, internalPort)
	}

	// Bail before localIP's interface enumeration; runWithCtx only checks ctx
	// once it is reached, so an already-cancelled caller would otherwise pay
	// for work whose result is discarded.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	internalIP, err := localIP()
	if err != nil {
		slog.Warn("portforward: could not determine the local address to map to", "err", err)
		return nil, fmt.Errorf("determine local ip: %w", err)
	}

	const requestedLease uint32 = 3600
	// externalPort defaults to internalPort. If the router already has that
	// port mapped to someone else, AddPortMapping fails and the caller can
	// retry with a different internalPort.
	externalPort := internalPort
	client := f.client
	m := &Mapping{
		ExternalPort:  externalPort,
		InternalPort:  internalPort,
		InternalIP:    internalIP,
		Protocol:      protocol,
		LeaseDuration: time.Duration(requestedLease) * time.Second,
		Method:        f.method,
		description:   description,
		requested:     requestedLease,
	}
	// The add can't be cancelled. When the caller gives up, or the gateway is
	// too slow, return at once rather than hold f.mu: m is then never
	// recorded, and addLocked removes whatever the add created once it
	// resolves.
	op := f.addLocked(client, m, description)
	select {
	case err = <-op.result:
		// f.mu is held throughout, so the goroutine can't have dealt with it.
		f.claim(op)
	case <-ctx.Done():
		slog.Warn("portforward: caller gave up while the gateway was adding the mapping; removing it once the add resolves",
			protocolAttr(protocol), "external_port", externalPort, "internal_port", internalPort, "err", ctx.Err())
		return nil, fmt.Errorf("add port mapping: %w", ctx.Err())
	case <-time.After(addCallTimeout):
		err = fmt.Errorf("add port mapping timed out after %s", addCallTimeout)
		slog.Warn("portforward: gateway did not answer in time; removing any mapping it creates once the add resolves",
			protocolAttr(protocol), "external_port", externalPort, "internal_port", internalPort,
			"internal_ip", internalIP, "method", f.method, "err", err)
		// Still the "this network can't host a peer" sentinel: the caller has
		// no mapping either way, and the retry decision is the same.
		return nil, fmt.Errorf("add port mapping: %w", errors.Join(ErrNoPortForwarding, err))
	}
	if ctxErr := ctx.Err(); ctxErr != nil && mayHaveLanded(err) {
		// The add resolved just as the caller gave up. Remove what it may have
		// created — otherwise it survives with no Forwarder tracking it.
		slog.Warn("portforward: caller gave up while the gateway was accepting the mapping; removing it",
			protocolAttr(protocol), "external_port", externalPort, "internal_port", internalPort, "err", ctxErr)
		f.removeLocked(m, client, "caller gave up while the gateway was accepting it")
		return nil, fmt.Errorf("add port mapping: %w", ctxErr)
	}
	if err != nil && mayHaveLanded(err) {
		// The add failed in a way that leaves it unknown whether the gateway
		// made the mapping, and it won't be tracked.
		f.removeLocked(m, client, "an add whose outcome is unknown")
	}
	if err != nil {
		// Propagate ctx cancellation/deadline verbatim so callers can retry
		// rather than treating it as a permanent "this network won't work".
		if ctxErr := ctx.Err(); ctxErr != nil {
			slog.Warn("portforward: adding the mapping was canceled or timed out",
				protocolAttr(protocol), "external_port", externalPort, "internal_port", internalPort,
				"internal_ip", internalIP, "method", f.method, "err", ctxErr)
			return nil, fmt.Errorf("add port mapping: %w", ctxErr)
		}
		slog.Warn("portforward: gateway refused the mapping",
			protocolAttr(protocol), "external_port", externalPort, "internal_port", internalPort,
			"internal_ip", internalIP, "method", f.method, "err", err)
		// Per the ErrNoPortForwarding docstring, a gateway refusing to map a
		// port is the "this network can't host a peer" case. Join the
		// sentinel so callers can detect it via errors.Is while still
		// surfacing the underlying router-specific reason for diagnostics.
		return nil, fmt.Errorf("add port mapping: %w", errors.Join(ErrNoPortForwarding, err))
	}

	if lr, ok := client.(leaseReporter); ok {
		if granted, ok := lr.lease(protocol, internalPort); ok {
			m.LeaseDuration = granted
		}
	}
	if f.mappings == nil {
		f.mappings = map[string]*Mapping{}
	}
	m.renewed = time.Now()
	f.mappings[protocol] = m
	if f.wake != nil {
		select {
		case f.wake <- struct{}{}:
		default:
		}
	}
	// Logged here rather than only by the caller on full success. A mapping that
	// the gateway accepts and then does not honour is invisible otherwise: the
	// caller's success line never runs because verification fails afterwards, so
	// the port and address we asked for — the two facts needed to check the
	// router's own table — were nowhere in the log. That is exactly the shape of
	// a 422 "could not connect to peer port".
	slog.Info("portforward: mapping added",
		protocolAttr(protocol), "external_port", externalPort, "internal_port", internalPort,
		"internal_ip", internalIP, "method", f.method,
		"requested_lease_s", requestedLease, "granted_lease_s", int(m.LeaseDuration/time.Second))
	// A copy, since renewals update the lease under f.mu.
	out := *m
	return &out, nil
}

// UnmapPort removes every active mapping. No-op if none is active.
// Always called as part of teardown — even if the gateway has already let
// the lease expire, DeletePortMapping is the polite signal to the router.
//
// A mapping is forgotten only on a successful delete. A failed delete leaves
// it in place so the caller can retry; otherwise we'd "forget" about a router
// rule that's actually still live and the user would have to wait for the
// UPnP lease to expire.
func (f *Forwarder) UnmapPort(ctx context.Context) error {
	// Defensive: callers that pass a *Forwarder through an interface (see
	// peer.Client's portForwarder shim) can land here with f == nil if a
	// failed construction collapsed `(*Forwarder)(nil), err` into a
	// non-nil-but-typed-nil interface. A bare `f.mu.Lock()` would panic;
	// this guard makes the cleanup path idempotent against that race.
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	// Counted before the delete and while holding the lock: a renewal blocked
	// on f.mu will observe it and undo anything it re-added.
	f.teardowns++
	client := f.client
	var errs []error
	for _, m := range f.activeMappings() {
		err := runWithCtx(ctx, func() error {
			return client.DeletePortMapping("", m.ExternalPort, m.Protocol)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("delete %s port mapping: %w", m.Protocol, err))
			continue
		}
		delete(f.mappings, m.Protocol)
	}
	return errors.Join(errs...)
}

// protocolAttr is a mapping's protocol as a log attribute, under OTel's key and
// in its lowercase form.
func protocolAttr(protocol string) slog.Attr {
	return slog.String(string(semconv.NetworkProtocolNameKey), strings.ToLower(protocol))
}

// activeMappings returns the mappings in protocol order, so teardown and
// renewal visit them in the same order every time. f.mu must be held.
func (f *Forwarder) activeMappings() []*Mapping {
	ms := make([]*Mapping, 0, len(f.mappings))
	for _, m := range f.mappings {
		ms = append(ms, m)
	}
	slices.SortFunc(ms, func(a, b *Mapping) int { return cmp.Compare(a.Protocol, b.Protocol) })
	return ms
}

// StartRenewal launches a goroutine that re-issues AddPortMapping for every
// mapping at half the requested lease duration (minimum 1 minute) until ctx is
// cancelled or UnmapPort is called. Mappings added after it starts are renewed
// too. The cadence is keyed off the lease each mapping records
// (mapping.LeaseDuration). NAT-PMP and PCP gateways say what they granted,
// and renewal comes sooner for a shorter grant. UPnP IGD has no API to query
// what the router actually assigned, so for UPnP it is what we *requested*,
// and a router that ignored the request and silently applied a shorter TTL
// can still drop the mapping between renewal ticks.
// The peer's heartbeat path will surface that failure and auto-Stop the
// session; routine 30-minute refresh of an hour-long requested lease
// handles the common case where the router honors the requested duration.
const (
	// renewCallTimeout bounds how long a renewal waits for the gateway before
	// giving up on learning whether the mapping was re-added. SOAP to a LAN
	// gateway normally answers in milliseconds.
	renewCallTimeout = 30 * time.Second
)

// addCallTimeout is the same bound as renewCallTimeout for the initial MapPort
// call. Derived from it rather than repeating the literal, so the two cannot
// drift. A var rather than a const only so tests can shorten it; nothing
// outside this package writes it.
var addCallTimeout = renewCallTimeout

func (f *Forwarder) StartRenewal(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		return
	}
	ms := f.activeMappings()
	if len(ms) == 0 {
		return
	}
	renewCtx, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	go f.renewLoop(renewCtx, baseRenewInterval(ms[0]))
}

// baseRenewInterval is renewLoop's interval: half the lease m asked for, and
// at least a minute. Every mapping asks for the same lease. nextRenewal comes
// sooner for a gateway that grants less; basing this on what was granted
// would cap renewal by the first grant for good.
func baseRenewInterval(m *Mapping) time.Duration {
	return max(time.Duration(m.requested)*time.Second/2, time.Minute)
}

// addOp is an AddPortMapping in flight for one mapping.
type addOp struct {
	result <-chan error
	key    portKey
	// handled is set, under f.mu, by whichever of the caller (claim) and the
	// goroutine making the call first deals with the add's outcome.
	handled bool
}

// addLocked issues the AddPortMapping for m, the mapping it is for. The
// caller that receives the result claims it (see claim) and deals with it.
// A caller may stop waiting instead; then, whenever the add resolves, it
// checks under f.mu whether m is the mapping tracked for its protocol. If not —
// the add was abandoned, or UnmapPort tore m down meanwhile — and the add may
// have created the mapping, it deletes it, leaving the port alone if the
// Forwarder has since mapped it again.
//
// An add that hasn't resolved after abandonedAddGrace gets a delete anyway,
// if m isn't tracked by then, and another once it resolves. The port counts as
// in flight until all that is done, and MapPort refuses it. f.mu must be held.
func (f *Forwarder) addLocked(client igdClient, m *Mapping, description string) *addOp {
	ch := make(chan error, 1)
	op := &addOp{result: ch, key: portKey{m.Protocol, m.ExternalPort}}
	f.holdLocked(op.key)
	var resolved atomic.Bool
	grace := time.AfterFunc(abandonedAddGrace, func() {
		f.mu.Lock()
		gone := !resolved.Load() && f.untrackedLocked(m)
		f.mu.Unlock()
		if gone {
			f.deleteUntracked(m, client, "an add the gateway never answered")
		}
	})
	go func() {
		err := client.AddPortMapping("", m.ExternalPort, m.Protocol, m.InternalPort, m.InternalIP, true, description, m.requested)
		resolved.Store(true)
		grace.Stop()
		ch <- err
		f.mu.Lock()
		if op.handled {
			f.mu.Unlock()
			return
		}
		op.handled = true
		gone := mayHaveLanded(err) && f.untrackedLocked(m)
		f.mu.Unlock()
		if gone {
			f.deleteUntracked(m, client, "an add that resolved after its mapping was abandoned or torn down")
		}
		f.mu.Lock()
		f.releaseLocked(op.key)
		f.mu.Unlock()
	}()
	return op
}

// claim takes over op's outcome for a caller that received its result, and
// reports whether it did; false means the goroutine already dealt with it.
// f.mu must be held.
func (f *Forwarder) claim(op *addOp) bool {
	if op.handled {
		return false
	}
	op.handled = true
	f.releaseLocked(op.key)
	return true
}

// removeLocked deletes m's port, which the Forwarder doesn't track, in the
// background, holding the port until the delete returns. f.mu must be held.
func (f *Forwarder) removeLocked(m *Mapping, client igdClient, why string) {
	k := portKey{m.Protocol, m.ExternalPort}
	f.holdLocked(k)
	go func() {
		f.deleteUntracked(m, client, why)
		f.mu.Lock()
		f.releaseLocked(k)
		f.mu.Unlock()
	}()
}

// untrackedLocked reports whether m is not the tracked mapping for its
// protocol and no other tracked mapping uses its port. f.mu must be held.
func (f *Forwarder) untrackedLocked(m *Mapping) bool {
	cur := f.mappings[m.Protocol]
	return cur != m && (cur == nil || cur.ExternalPort != m.ExternalPort)
}

func (f *Forwarder) holdLocked(k portKey) {
	if f.inflight == nil {
		f.inflight = map[portKey]int{}
	}
	f.inflight[k]++
}

func (f *Forwarder) releaseLocked(k portKey) {
	if f.inflight[k]--; f.inflight[k] <= 0 {
		delete(f.inflight, k)
	}
}

func (f *Forwarder) renewTimeout() time.Duration {
	if f.renewWait > 0 {
		return f.renewWait
	}
	return renewCallTimeout
}

// abandonedAddGrace is how long an add may go unresolved before its mapping,
// if no longer tracked, is deleted anyway. A var only so tests can shorten it.
var abandonedAddGrace = 2 * time.Minute

// deleteUntracked deletes m's port on the gateway, waiting for the answer so
// addLocked holds the port until the delete is done. Best effort: the only
// remaining recourse is the router's own lease expiry.
func (f *Forwarder) deleteUntracked(m *Mapping, client igdClient, why string) {
	if err := client.DeletePortMapping("", m.ExternalPort, m.Protocol); err != nil {
		slog.Warn("portforward: could not remove an untracked mapping",
			"err", err, protocolAttr(m.Protocol), "external_port", m.ExternalPort, "why", why)
		return
	}
	slog.Info("portforward: removed an untracked mapping",
		protocolAttr(m.Protocol), "external_port", m.ExternalPort, "why", why)
}

// renewLoop renews every mapping once any is due: each interval after it was
// last renewed, or sooner when a gateway granted a lease shorter than twice
// that. A mapping added meanwhile reschedules it.
func (f *Forwarder) renewLoop(ctx context.Context, interval time.Duration) {
	f.mu.Lock()
	if f.wake == nil {
		f.wake = make(chan struct{}, 1)
	}
	wake := f.wake
	f.mu.Unlock()
	for {
		t := time.NewTimer(f.nextRenewal(interval))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
			t.Stop()
		case <-t.C:
			f.mu.Lock()
			ms := f.activeMappings()
			client := f.client
			teardowns := f.teardowns
			f.mu.Unlock()
			if len(ms) == 0 {
				return
			}
			for _, m := range ms {
				// A renewal in flight finishes and does its teardown check, but
				// once ctx ends no further protocol is renewed.
				if ctx.Err() != nil || !f.renew(m, client, teardowns) {
					return
				}
			}
		}
	}
}

// nextRenewal is how long renewLoop waits before the first mapping is due.
func (f *Forwarder) nextRenewal(interval time.Duration) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mappings) == 0 {
		return interval
	}
	var next time.Time
	for _, m := range f.mappings {
		due := m.renewed.Add(renewPeriod(m, interval))
		if due.Before(m.retryAt) {
			due = m.retryAt
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return max(time.Until(next), 0)
}

// renewRetryDelay is how long after a failed renewal it is retried: an eighth
// of the lease, so a few tries fit before it runs out, but at least a second
// and at most a minute.
func renewRetryDelay(m *Mapping) time.Duration {
	return min(max(m.LeaseDuration/8, time.Second), time.Minute)
}

// renewPeriod is how often m is renewed: interval, or half the lease the
// gateway granted if that's sooner.
func renewPeriod(m *Mapping, interval time.Duration) time.Duration {
	return min(interval, max(m.LeaseDuration/2, minRenewInterval))
}

// renew re-issues m's AddPortMapping, and reports false if UnmapPort ran
// since the caller read teardowns, after removing what the renewal may have
// re-added. The caller reads teardowns along with m, so a teardown between the
// two can't go unnoticed.
func (f *Forwarder) renew(m *Mapping, client igdClient, teardowns uint64) bool {
	// Most routers treat a re-issued AddPortMapping as "extend the
	// existing lease"; some replace it with a fresh one. Either is
	// fine here.
	//
	// addLocked removes the mapping again if, by the time the add resolves
	// however late, a teardown has untracked it. The wait here is bounded by
	// renewCallTimeout only so a wedged gateway can't stall the loop.
	f.mu.Lock()
	if f.mappings[m.Protocol] != m {
		// Torn down since the loop's snapshot: nothing to renew.
		f.mu.Unlock()
		return false
	}
	op := f.addLocked(client, m, m.description+" (renew)")
	f.mu.Unlock()
	var confirmed, received bool
	var addErr error
	select {
	case addErr = <-op.result:
		received = true
		confirmed = addErr == nil
		if addErr != nil {
			slog.Warn("portforward: lease renewal failed", "err", addErr, protocolAttr(m.Protocol), "external_port", m.ExternalPort)
		}
	case <-time.After(f.renewTimeout()):
		slog.Warn("portforward: lease renewal timed out; it is removed if teardown runs before it resolves",
			protocolAttr(m.Protocol), "external_port", m.ExternalPort, "timeout", f.renewTimeout())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	tornDown := f.teardowns != teardowns
	if received && f.claim(op) && mayHaveLanded(addErr) && f.untrackedLocked(m) {
		// UnmapPort deleted m while the renewal was in flight, and the renewal
		// may have re-added it.
		f.removeLocked(m, client, "re-added by an in-flight renewal")
	}
	// Only a confirmed renewal extends the lease. Otherwise retry soon,
	// within what's left of it.
	if confirmed {
		m.renewed, m.retryAt = time.Now(), time.Time{}
	} else {
		m.retryAt = time.Now().Add(renewRetryDelay(m))
	}
	if lr, ok := client.(leaseReporter); ok && !tornDown && confirmed {
		// The gateway may grant a different lease this time.
		if granted, ok := lr.lease(m.Protocol, m.InternalPort); ok {
			m.LeaseDuration = granted
		}
	}
	return !tornDown
}

// mayHaveLanded reports whether an add that returned err may have created its
// mapping: it succeeded, or its outcome is unknown.
func mayHaveLanded(err error) bool {
	return err == nil || errors.Is(err, errOutcomeUnknown)
}

// ExternalIP queries the gateway for its WAN-side IP address. Cheaper than
// dialing a public-IP service when we already have a UPnP client open.
func (f *Forwarder) ExternalIP(ctx context.Context) (string, error) {
	f.mu.Lock()
	c := f.client
	f.mu.Unlock()
	var ip string
	err := runWithCtx(ctx, func() error {
		got, gerr := c.GetExternalIPAddress()
		if gerr != nil {
			return gerr
		}
		ip = got
		return nil
	})
	if err != nil {
		slog.Warn("portforward: gateway would not report its external address",
			"method", f.method, "err", err)
		return "", fmt.Errorf("get external ip: %w", err)
	}
	if ip == "" {
		slog.Warn("portforward: gateway reported an empty external address", "method", f.method)
		return "", fmt.Errorf("gateway returned empty external ip")
	}
	return ip, nil
}

// localIP returns the LAN address the OS would use to reach the gateway.
//
// First tries the UDP-noop trick (let the kernel pick a route to a known
// public address) — fastest and most accurate when the host has a working
// default route. Falls back to scanning interfaces for a private IPv4 if
// that fails, which covers networks that block 8.8.8.8 outbound or use
// non-default IPv4 routing tables. UPnP IGD itself is IPv4 in IGDv1 and
// almost always IPv4 in IGDv2, so we only consider IPv4 addresses.
func localIP() (string, error) {
	if ip, err := localIPByDial(); err == nil {
		return ip, nil
	}
	return localIPByInterfaceScan()
}

func localIPByDial() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "", fmt.Errorf("dial udp for local ip: %w", err)
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected local addr type %T", conn.LocalAddr())
	}
	return addr.IP.String(), nil
}

func localIPByInterfaceScan() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no usable private ipv4 found on any interface")
}

func LocalIP() (string, error) { return localIP() }

// runWithCtx wraps a blocking call so the caller's context can abort the
// wait. Returns ctx.Err() immediately if ctx is already cancelled, so the
// gateway-side side effect (port mapping, etc.) doesn't fire after the
// caller has already given up. If ctx cancels mid-call, the wrapped
// goroutine still runs to completion — UPnP/HTTP calls have their own
// underlying timeouts — but we no longer hand the entire wait time to an
// unresponsive gateway.
func runWithCtx(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func discoverIGDv2(ctx context.Context) (igdClient, error) {
	clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	return wanIPv2Wrapper{c: clients[0]}, nil
}

// discoverIGDv1 looks for both WANIPConnection and WANPPPConnection gateways.
// Cable/fiber CPE routers typically expose UPnP via WANIPConnection; DSL and
// other PPPoE-terminated CPEs typically expose it via WANPPPConnection.
// Probing only one would miss large swaths of consumer hardware.
func discoverIGDv1(ctx context.Context) (igdClient, error) {
	if clients, _, err := internetgateway1.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return wanIPv1Wrapper{c: clients[0]}, nil
	}
	clients, _, err := internetgateway1.NewWANPPPConnection1ClientsCtx(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	return wanPPPv1Wrapper{c: clients[0]}, nil
}

// upnpOutcome marks an AddPortMapping error as an unknown outcome unless it is
// a SOAP fault, the gateway's refusal. A transport or decoding error can come
// after the gateway has acted on the request.
func upnpOutcome(err error) error {
	var fault *soap.SOAPFaultError
	if err == nil || errors.As(err, &fault) {
		return err
	}
	return fmt.Errorf("%w: %w", errOutcomeUnknown, err)
}

// IGDv1 and IGDv2's generated clients have slightly different method
// signatures, so wrappers normalize them to a single igdClient interface.

type wanIPv2Wrapper struct {
	c *internetgateway2.WANIPConnection2
}

func (w wanIPv2Wrapper) AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return upnpOutcome(w.c.AddPortMapping(remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration))
}
func (w wanIPv2Wrapper) DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error {
	return w.c.DeletePortMapping(remoteHost, externalPort, protocol)
}
func (w wanIPv2Wrapper) GetExternalIPAddress() (string, error) {
	return w.c.GetExternalIPAddress()
}

type wanIPv1Wrapper struct {
	c *internetgateway1.WANIPConnection1
}

func (w wanIPv1Wrapper) AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return upnpOutcome(w.c.AddPortMapping(remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration))
}
func (w wanIPv1Wrapper) DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error {
	return w.c.DeletePortMapping(remoteHost, externalPort, protocol)
}
func (w wanIPv1Wrapper) GetExternalIPAddress() (string, error) {
	return w.c.GetExternalIPAddress()
}

type wanPPPv1Wrapper struct {
	c *internetgateway1.WANPPPConnection1
}

func (w wanPPPv1Wrapper) AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return upnpOutcome(w.c.AddPortMapping(remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration))
}
func (w wanPPPv1Wrapper) DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error {
	return w.c.DeletePortMapping(remoteHost, externalPort, protocol)
}
func (w wanPPPv1Wrapper) GetExternalIPAddress() (string, error) {
	return w.c.GetExternalIPAddress()
}

var (
	_ igdClient = wanIPv2Wrapper{}
	_ igdClient = wanIPv1Wrapper{}
	_ igdClient = wanPPPv1Wrapper{}
)
