// Package pool provides the pool DNS server: it answers from whichever of
// several other DNS servers answers first, so no single resolver that a
// network blocks can take DNS down with it.
package pool

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

const (
	defaultStagger = 250 * time.Millisecond
	defaultTimeout = 5 * time.Second
)

func RegisterTransport(registry *dns.TransportRegistry) {
	dns.RegisterTransport[option.DNSPoolServerOptions](registry, constant.TypeDNSPool, NewTransport)
}

var (
	_ adapter.DNSTransport                = (*Transport)(nil)
	_ adapter.DNSTransportWithEnvironment = (*Transport)(nil)
)

// Transport tries its servers in order, starting the next one each time the
// stagger passes or a server fails, and answers with the first acceptable
// response. The server that answered last is tried first, so a resolver that
// works costs one exchange, and one that stops working costs one stagger.
// Reset, which follows a network change, forgets it.
type Transport struct {
	dns.TransportAdapter
	ctx     context.Context
	logger  log.ContextLogger
	servers []string
	stagger time.Duration
	timeout time.Duration
	// winner is the index in servers of the server that answered last, or -1.
	winner atomic.Int32
}

func NewTransport(ctx context.Context, logger log.ContextLogger, tag string, options option.DNSPoolServerOptions) (adapter.DNSTransport, error) {
	if len(options.Servers) == 0 {
		return nil, E.New("missing servers")
	}
	var servers []string
	for _, server := range options.Servers {
		if server == "" {
			return nil, E.New("empty server tag")
		}
		if server == tag {
			return nil, E.New("pool includes itself")
		}
		if !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	}
	t := &Transport{
		TransportAdapter: dns.NewTransportAdapter(constant.TypeDNSPool, tag, servers),
		ctx:              ctx,
		logger:           logger,
		servers:          servers,
		stagger:          time.Duration(options.Stagger),
		timeout:          time.Duration(options.Timeout),
	}
	if t.stagger <= 0 {
		t.stagger = defaultStagger
	}
	if t.timeout <= 0 {
		t.timeout = defaultTimeout
	}
	t.winner.Store(-1)
	return t, nil
}

// Start does nothing. The manager checks the servers exist (Dependencies),
// and looking them up here would deadlock a pool created at runtime: the
// manager holds its lock while it starts one.
func (t *Transport) Start(adapter.StartStage) error {
	return nil
}

func (t *Transport) Close() error {
	return nil
}

// Environment identifies the servers' environments, so the DNS client keys
// the pool's cached answers to them and to the network, as it would each
// server's: answers from before a network change aren't reused after it.
func (t *Transport) Environment() []string {
	return t.environment(nil)
}

// environment is Environment for a pool reached through the pools in
// visiting, which it doesn't enter again: a server replaced at runtime
// isn't checked for cycles, as servers are at startup.
func (t *Transport) environment(visiting []string) []string {
	manager := service.FromContext[adapter.DNSTransportManager](t.ctx)
	if manager == nil || slices.Contains(visiting, t.Tag()) {
		return nil
	}
	visiting = slices.Clip(append(slices.Clip(visiting), t.Tag()))
	var environment []string
	for _, tag := range t.servers {
		server, found := manager.Transport(tag)
		if !found {
			continue
		}
		var entries []string
		if pool, ok := server.(*Transport); ok {
			entries = pool.environment(visiting)
		} else if withEnvironment, ok := server.(adapter.DNSTransportWithEnvironment); ok {
			entries = withEnvironment.Environment()
		}
		for _, entry := range entries {
			environment = append(environment, tag+"="+entry)
		}
	}
	return environment
}

// visitingKey keys the tags of the pools an exchange has passed through.
type visitingKey struct{}

// Reset forgets the last winner. The servers reset their own connections.
func (t *Transport) Reset() {
	t.winner.Store(-1)
}

type result struct {
	index    int
	response *mDNS.Msg
	err      error
}

func (t *Transport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	// Servers are looked up for each exchange, so a server the manager
	// replaces is used as it is now.
	manager := service.FromContext[adapter.DNSTransportManager](t.ctx)
	if manager == nil {
		return nil, E.New("missing DNS transport manager")
	}
	// A pool replaced at runtime can form a cycle the manager doesn't check
	// for; an exchange that comes back to this pool fails at once. (A
	// member's own lookups through the router that come back here are
	// already refused by sing-box's DNS client as a loopback.)
	visiting, _ := ctx.Value(visitingKey{}).([]string)
	if slices.Contains(visiting, t.Tag()) {
		return nil, E.New("DNS pool cycle: ", strings.Join(visiting, " -> "), " -> ", t.Tag())
	}
	// Members share the chain, so it is never appended to in place: every
	// append starts from a clipped slice, and the stored one is clipped too.
	ctx = context.WithValue(ctx, visitingKey{}, slices.Clip(append(slices.Clip(visiting), t.Tag())))
	order := t.order()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(order))
	start := func(index int) {
		server, found := manager.Transport(t.servers[index])
		if !found {
			results <- result{index: index, err: E.New("DNS server not found: ", t.servers[index])}
			return
		}
		// A fake IP answered through a pool would be taken as real: the
		// router recognizes fake answers by the answering server's type.
		if server.Type() == C.DNSTypeFakeIP {
			results <- result{index: index, err: E.New("fakeip server in a pool: ", t.servers[index])}
			return
		}
		// Copied before the worker starts: Exchange may return, and its
		// caller reuse the message, before the worker runs.
		request := message.Copy()
		go func() {
			exchangeCtx, exchangeCancel := context.WithTimeout(ctx, t.timeout)
			defer exchangeCancel()
			response, err := server.Exchange(exchangeCtx, request)
			// A transport may report a DNS status as an error, as sing-box's
			// DNS client expects; it is an answer like any other.
			var rcodeError dns.RcodeError
			if errors.As(err, &rcodeError) {
				response, err = dns.FixedResponseStatus(request, int(rcodeError)), nil
			}
			results <- result{index: index, response: response, err: err}
		}()
	}

	next, pending := 0, 0
	launch := func() {
		start(order[next])
		next++
		pending++
	}
	launch()
	stagger := time.NewTimer(t.stagger)
	defer stagger.Stop()
	var (
		errs     []error
		fallback *mDNS.Msg
	)
	for pending > 0 {
		select {
		case <-ctx.Done():
			if fallback != nil {
				return fallback, nil
			}
			return nil, E.Cause(errors.Join(append(errs, ctx.Err())...), "DNS pool")
		case <-stagger.C:
			if next < len(order) {
				launch()
				stagger.Reset(t.stagger)
			}
		case r := <-results:
			pending--
			if r.err == nil && acceptable(r.response) {
				// Only records make a server the winner: a censor's or a
				// hosts server's resolver answers NXDOMAIN or no records
				// fast, and must not go first for every later query.
				if r.response.Rcode == mDNS.RcodeSuccess && len(r.response.Answer) > 0 {
					t.setWinner(r.index)
				}
				return r.response, nil
			}
			if r.err != nil {
				errs = append(errs, E.Cause(r.err, t.servers[r.index]))
			} else if fallback == nil {
				fallback = r.response
			}
			// A failure starts the next server now rather than after
			// the stagger.
			if next < len(order) {
				launch()
				stagger.Reset(t.stagger)
			}
		}
	}
	// No server gave an acceptable answer. One that answered at all, such as
	// a SERVFAIL every server agrees on, is better than an error.
	if fallback != nil {
		return fallback, nil
	}
	return nil, E.Cause(errors.Join(errs...), "all DNS servers failed")
}

func (t *Transport) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(response *mDNS.Msg, err error)) {
	go func() {
		callback(t.Exchange(ctx, message))
	}()
}

// order is the servers' indexes in the order to try them: the last winner,
// then the rest as configured.
func (t *Transport) order() []int {
	order := make([]int, 0, len(t.servers))
	winner := int(t.winner.Load())
	if winner >= 0 && winner < len(t.servers) {
		order = append(order, winner)
	}
	for i := range t.servers {
		if i != winner {
			order = append(order, i)
		}
	}
	return order
}

func (t *Transport) setWinner(index int) {
	if previous := t.winner.Swap(int32(index)); previous != int32(index) {
		t.logger.Debug("answering from ", t.servers[index])
	}
}

// acceptable reports whether a response settles the query. A server that
// fails or refuses it does not: another server may answer.
func acceptable(response *mDNS.Msg) bool {
	if response == nil {
		return false
	}
	switch response.Rcode {
	case mDNS.RcodeSuccess, mDNS.RcodeNameError:
		return true
	}
	return false
}
