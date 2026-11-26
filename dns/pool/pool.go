// Package pool provides the pool DNS server: it answers from whichever of
// several other DNS servers answers first, so no single resolver that a
// network blocks can take DNS down with it.
package pool

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
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

var _ adapter.DNSTransport = (*Transport)(nil)

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

func (t *Transport) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	manager := service.FromContext[adapter.DNSTransportManager](t.ctx)
	if manager == nil {
		return E.New("missing DNS transport manager")
	}
	for _, server := range t.servers {
		if _, found := manager.Transport(server); !found {
			return E.New("DNS server not found: ", server)
		}
	}
	return nil
}

func (t *Transport) Close() error {
	return nil
}

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
		go func() {
			exchangeCtx, exchangeCancel := context.WithTimeout(ctx, t.timeout)
			defer exchangeCancel()
			response, err := server.Exchange(exchangeCtx, message.Copy())
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
			return nil, ctx.Err()
		case <-stagger.C:
			if next < len(order) {
				launch()
				stagger.Reset(t.stagger)
			}
		case r := <-results:
			pending--
			if r.err == nil && acceptable(r.response) {
				t.setWinner(r.index)
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
