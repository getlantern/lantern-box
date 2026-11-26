package pool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// fakeServer answers after delay with rcode, or fails with err.
type fakeServer struct {
	dns.TransportAdapter
	delay time.Duration
	rcode int
	err   error
	calls atomic.Int32
}

func newFakeServer(tag string, delay time.Duration, rcode int, err error) *fakeServer {
	return &fakeServer{TransportAdapter: dns.NewTransportAdapter("fake", tag, nil), delay: delay, rcode: rcode, err: err}
}

func (s *fakeServer) Start(adapter.StartStage) error { return nil }
func (s *fakeServer) Close() error                   { return nil }
func (s *fakeServer) Reset()                         {}

func (s *fakeServer) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	s.calls.Add(1)
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	response := new(mDNS.Msg)
	response.SetRcode(message, s.rcode)
	response.Answer = []mDNS.RR{&mDNS.TXT{Hdr: mDNS.RR_Header{Name: message.Question[0].Name, Rrtype: mDNS.TypeTXT, Class: mDNS.ClassINET}, Txt: []string{s.Tag()}}}
	return response, nil
}

func (s *fakeServer) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(*mDNS.Msg, error)) {
	callback(s.Exchange(ctx, message))
}

type fakeManager struct {
	adapter.DNSTransportManager
	servers map[string]adapter.DNSTransport
}

func (m *fakeManager) Transport(tag string) (adapter.DNSTransport, bool) {
	s, ok := m.servers[tag]
	return s, ok
}

const stagger = 100 * time.Millisecond

func newPool(t *testing.T, servers ...*fakeServer) *Transport {
	t.Helper()
	m := &fakeManager{servers: map[string]adapter.DNSTransport{}}
	var tags []string
	for _, s := range servers {
		m.servers[s.Tag()] = s
		tags = append(tags, s.Tag())
	}
	ctx := service.ContextWith[adapter.DNSTransportManager](context.Background(), m)
	tr, err := NewTransport(ctx, log.NewNOPFactory().Logger(), "pool", option.DNSPoolServerOptions{
		Servers: tags,
		Stagger: badoption.Duration(stagger),
		Timeout: badoption.Duration(2 * time.Second),
	})
	require.NoError(t, err)
	require.NoError(t, tr.Start(adapter.StartStateStart))
	return tr.(*Transport)
}

func query() *mDNS.Msg {
	m := new(mDNS.Msg)
	m.SetQuestion("example.com.", mDNS.TypeA)
	return m
}

// from names the server that answered.
func from(t *testing.T, response *mDNS.Msg) string {
	t.Helper()
	require.NotNil(t, response)
	require.Len(t, response.Answer, 1)
	return response.Answer[0].(*mDNS.TXT).Txt[0]
}

func TestFirstServerAnswersAlone(t *testing.T) {
	a := newFakeServer("a", 0, mDNS.RcodeSuccess, nil)
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, "a", from(t, response))
	assert.Zero(t, b.calls.Load(), "the next server waits for the stagger")
}

func TestStaggerStartsTheNextServerAndItWinsAfter(t *testing.T) {
	a := newFakeServer("a", time.Second, mDNS.RcodeSuccess, nil)
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	started := time.Now()
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, "b", from(t, response))
	assert.Less(t, time.Since(started), time.Second, "a slow server doesn't hold the answer")

	a.calls.Store(0)
	response, err = p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, "b", from(t, response))
	assert.Zero(t, a.calls.Load(), "the last winner is tried first")
}

func TestFailureStartsTheNextServerAtOnce(t *testing.T) {
	a := newFakeServer("a", 0, 0, errors.New("blocked"))
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	started := time.Now()
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, "b", from(t, response))
	assert.Less(t, time.Since(started), stagger, "no stagger after a failure")
}

func TestRefusalIsNotAnAnswer(t *testing.T) {
	a := newFakeServer("a", 0, mDNS.RcodeServerFailure, nil)
	b := newFakeServer("b", 0, mDNS.RcodeNameError, nil)
	p := newPool(t, a, b)
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, "b", from(t, response), "NXDOMAIN settles the query, SERVFAIL doesn't")
}

func TestAllRefusingReturnsARefusal(t *testing.T) {
	a := newFakeServer("a", 0, mDNS.RcodeServerFailure, nil)
	b := newFakeServer("b", 0, 0, errors.New("blocked"))
	p := newPool(t, a, b)
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, mDNS.RcodeServerFailure, response.Rcode)
}

func TestAllFailingFails(t *testing.T) {
	a := newFakeServer("a", 0, 0, errors.New("blocked a"))
	b := newFakeServer("b", 0, 0, errors.New("blocked b"))
	p := newPool(t, a, b)
	_, err := p.Exchange(context.Background(), query())
	require.Error(t, err)
	assert.ErrorContains(t, err, "blocked a")
	assert.ErrorContains(t, err, "blocked b")
}

func TestResetForgetsTheWinner(t *testing.T) {
	a := newFakeServer("a", 0, 0, errors.New("blocked"))
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	_, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, []int{1, 0}, p.order())
	p.Reset()
	assert.Equal(t, []int{0, 1}, p.order(), "after a network change the configured order applies again")
}

func TestCallerCancelStopsTheExchange(t *testing.T) {
	a := newFakeServer("a", time.Second, mDNS.RcodeSuccess, nil)
	p := newPool(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Exchange(ctx, query())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestOptionsAreChecked(t *testing.T) {
	ctx := context.Background()
	logger := log.NewNOPFactory().Logger()
	_, err := NewTransport(ctx, logger, "pool", option.DNSPoolServerOptions{})
	assert.ErrorContains(t, err, "missing servers")
	_, err = NewTransport(ctx, logger, "pool", option.DNSPoolServerOptions{Servers: []string{"a", "pool"}})
	assert.ErrorContains(t, err, "itself")

	m := &fakeManager{servers: map[string]adapter.DNSTransport{}}
	tr, err := NewTransport(service.ContextWith[adapter.DNSTransportManager](ctx, m), logger, "pool",
		option.DNSPoolServerOptions{Servers: []string{"missing"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"missing"}, tr.Dependencies(), "the manager checks the servers exist and starts them first")
	_, err = tr.Exchange(ctx, query())
	assert.ErrorContains(t, err, "not found")
}

func TestNXDOMAINIsReturnedButDoesNotWin(t *testing.T) {
	a := newFakeServer("a", 0, mDNS.RcodeNameError, nil)
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, mDNS.RcodeNameError, response.Rcode)
	assert.Equal(t, []int{0, 1}, p.order(), "a fast NXDOMAIN, such as a censor's, must not go first for every query")
}

type fakeIPServer struct{ *fakeServer }

func (fakeIPServer) Type() string { return C.DNSTypeFakeIP }

func TestFakeIPServerIsNotUsed(t *testing.T) {
	fake := fakeIPServer{newFakeServer("fake", 0, mDNS.RcodeSuccess, nil)}
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	m := &fakeManager{servers: map[string]adapter.DNSTransport{"fake": fake, "b": b}}
	tr, err := NewTransport(service.ContextWith[adapter.DNSTransportManager](context.Background(), m),
		log.NewNOPFactory().Logger(), "pool", option.DNSPoolServerOptions{Servers: []string{"fake", "b"}})
	require.NoError(t, err)
	response, err := tr.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, "b", from(t, response))
	assert.Zero(t, fake.calls.Load(), "a fake answer through a pool would be taken as real")
}

func TestCallerCancelReturnsARefusalAlreadyReceived(t *testing.T) {
	a := newFakeServer("a", 0, mDNS.RcodeServerFailure, nil)
	b := newFakeServer("b", time.Second, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	response, err := p.Exchange(ctx, query())
	require.NoError(t, err)
	assert.Equal(t, mDNS.RcodeServerFailure, response.Rcode)
}

func TestEmptyAnswerDoesNotWin(t *testing.T) {
	a := newFakeServer("a", 0, 0, errors.New("blocked"))
	b := &emptyServer{newFakeServer("b", 0, mDNS.RcodeSuccess, nil)}
	m := &fakeManager{servers: map[string]adapter.DNSTransport{"a": a, "b": b}}
	tr, err := NewTransport(service.ContextWith[adapter.DNSTransportManager](context.Background(), m),
		log.NewNOPFactory().Logger(), "pool", option.DNSPoolServerOptions{Servers: []string{"a", "b"}})
	require.NoError(t, err)
	p := tr.(*Transport)
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Empty(t, response.Answer, "NODATA is still the answer")
	assert.Equal(t, []int{0, 1}, p.order(), "but no records make no winner")
}

// emptyServer answers NOERROR with no records.
type emptyServer struct{ *fakeServer }

func (s *emptyServer) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	response, err := s.fakeServer.Exchange(ctx, message)
	if response != nil {
		response.Answer = nil
	}
	return response, err
}

func TestRcodeErrorIsAnAnswer(t *testing.T) {
	a := newFakeServer("a", 0, 0, dns.RcodeNameError)
	b := newFakeServer("b", time.Second, mDNS.RcodeSuccess, nil)
	p := newPool(t, a, b)
	started := time.Now()
	response, err := p.Exchange(context.Background(), query())
	require.NoError(t, err)
	assert.Equal(t, mDNS.RcodeNameError, response.Rcode, "a status reported as an error settles the query")
	assert.Less(t, time.Since(started), time.Second)
}

type envServer struct {
	*fakeServer
	env []string
}

func (s *envServer) Environment() []string { return s.env }

func TestEnvironmentCombinesTheServers(t *testing.T) {
	a := &envServer{newFakeServer("a", 0, mDNS.RcodeSuccess, nil), []string{"wifi"}}
	b := newFakeServer("b", 0, mDNS.RcodeSuccess, nil)
	m := &fakeManager{servers: map[string]adapter.DNSTransport{"a": a, "b": b}}
	tr, err := NewTransport(service.ContextWith[adapter.DNSTransportManager](context.Background(), m),
		log.NewNOPFactory().Logger(), "pool", option.DNSPoolServerOptions{Servers: []string{"a", "b"}})
	require.NoError(t, err)
	env, ok := tr.(adapter.DNSTransportWithEnvironment)
	require.True(t, ok, "the DNS client keys cached answers to the network only for a transport with an environment")
	assert.Equal(t, []string{"a=wifi"}, env.Environment())
}

// TestCycleFailsInsteadOfRecursing builds two pools that list each other,
// as replacing a server at runtime can: the manager checks for cycles only
// at startup.
func TestCycleFailsInsteadOfRecursing(t *testing.T) {
	m := &fakeManager{servers: map[string]adapter.DNSTransport{}}
	ctx := service.ContextWith[adapter.DNSTransportManager](context.Background(), m)
	logger := log.NewNOPFactory().Logger()
	a, err := NewTransport(ctx, logger, "a", option.DNSPoolServerOptions{Servers: []string{"b"}})
	require.NoError(t, err)
	b, err := NewTransport(ctx, logger, "b", option.DNSPoolServerOptions{Servers: []string{"a"}})
	require.NoError(t, err)
	m.servers["a"], m.servers["b"] = a, b

	assert.Empty(t, a.(adapter.DNSTransportWithEnvironment).Environment(), "no stack overflow")
	started := time.Now()
	_, err = a.Exchange(context.Background(), query())
	assert.ErrorContains(t, err, "DNS pool cycle")
	assert.Less(t, time.Since(started), time.Second, "fails at once, not after timeouts")
}

// TestCycleThroughSeveralMembers checks a cycle reached through more than
// one member of a pool (c's members a and b both lead back) fails too.
func TestCycleThroughSeveralMembers(t *testing.T) {
	m := &fakeManager{servers: map[string]adapter.DNSTransport{}}
	ctx := service.ContextWith[adapter.DNSTransportManager](context.Background(), m)
	logger := log.NewNOPFactory().Logger()
	for tag, servers := range map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a", "b"}} {
		pool, err := NewTransport(ctx, logger, tag, option.DNSPoolServerOptions{Servers: servers, Stagger: badoption.Duration(time.Millisecond)})
		require.NoError(t, err)
		m.servers[tag] = pool
	}
	for range 20 {
		_, err := m.servers["a"].Exchange(context.Background(), query())
		assert.ErrorContains(t, err, "DNS pool cycle")
	}
}
