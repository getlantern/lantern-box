package group

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/constant"
)

// tcpPair returns both ends of a loopback TCP conn, or nil ends on failure so
// it is safe to call off the test goroutine.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Error(err)
		return nil, nil
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	dialed, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Error(err)
		return nil, nil
	}
	server := <-accepted
	t.Cleanup(func() {
		dialed.Close()
		if server != nil {
			server.Close()
		}
	})
	return dialed, server
}

// loopbackOutbound dials a fresh loopback conn per flow. When gate is set the
// dial signals started and waits for gate.
func loopbackOutbound(t *testing.T, tag string, started chan<- struct{}, gate <-chan struct{}) *mockOutbound {
	return &mockOutbound{
		tag: tag,
		dial: func(context.Context) (net.Conn, error) {
			if gate != nil {
				started <- struct{}{}
				<-gate
			}
			local, _ := tcpPair(t)
			if local == nil {
				return nil, errors.New("loopback dial failed")
			}
			return local, nil
		},
		listen: func(context.Context) (net.PacketConn, error) {
			return net.ListenPacket("udp", "127.0.0.1:0")
		},
	}
}

// handlerOutbound hands routed flows to the connection manager itself, as a
// nested group does; async dispatches from another goroutine. (Outbounds that
// copy flows themselves, like banditprobe, are not tracked.)
type handlerOutbound struct {
	*mockOutbound
	connMgr A.ConnectionManager
	async   bool
}

func (h *handlerOutbound) NewConnectionEx(ctx context.Context, conn net.Conn, md A.InboundContext, onClose N.CloseHandlerFunc) {
	if h.async {
		go h.connMgr.NewConnection(ctx, h.mockOutbound, conn, md, onClose)
		return
	}
	h.connMgr.NewConnection(ctx, h.mockOutbound, conn, md, onClose)
}

func newTestSelector(t *testing.T, members ...A.Outbound) *MutableSelector {
	t.Helper()
	s := &MutableSelector{
		Adapter:   outbound.NewAdapter(constant.TypeMutableSelector, "manual", nil, nil),
		ctx:       context.Background(),
		connMgr:   route.NewConnectionManager(log.NewNOPFactory().Logger()),
		logger:    log.NewNOPFactory().Logger(),
		outbounds: make(map[string]A.Outbound),
	}
	for _, m := range members {
		s.tags = append(s.tags, m.Tag())
		s.outbounds[m.Tag()] = m
	}
	s.storeSelected(members[0])
	return s
}

// routeFlow sends a TCP flow through s; the returned channel fires on teardown.
func routeFlow(t *testing.T, s *MutableSelector) chan error {
	_, inbound := tcpPair(t)
	closed := make(chan error, 1)
	s.NewConnectionEx(context.Background(), inbound, A.InboundContext{
		Destination: M.ParseSocksaddr("example.com:443"),
	}, func(err error) { closed <- err })
	return closed
}

func routePacketFlow(t *testing.T, s *MutableSelector) chan error {
	inbound, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { inbound.Close() })
	closed := make(chan error, 1)
	s.NewPacketConnectionEx(context.Background(), bufio.NewPacketConn(inbound), A.InboundContext{
		Destination: M.ParseSocksaddr("127.0.0.1:53"),
	}, func(err error) { closed <- err })
	return closed
}

func requireClosed(t *testing.T, closed chan error) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("flow on the old outbound was not closed")
	}
}

func requireOpen(t *testing.T, closed chan error) {
	t.Helper()
	select {
	case err := <-closed:
		t.Fatalf("flow closed unexpectedly: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMutableSelector_ReselectClosesRoutedFlows(t *testing.T) {
	s := newTestSelector(t, loopbackOutbound(t, "a", nil, nil), loopbackOutbound(t, "b", nil, nil))
	tcp, udp := routeFlow(t, s), routePacketFlow(t, s)
	requireOpen(t, tcp)
	requireOpen(t, udp)

	require.True(t, s.SelectOutbound("b"))
	requireClosed(t, tcp)
	requireClosed(t, udp)
	requireOpen(t, routeFlow(t, s))
}

func TestMutableSelector_SameSelectionKeepsFlows(t *testing.T) {
	s := newTestSelector(t, loopbackOutbound(t, "a", nil, nil))
	closed := routeFlow(t, s)

	require.True(t, s.SelectOutbound("a"))
	requireOpen(t, closed)
}

func TestMutableSelector_ReselectDuringDialClosesFlow(t *testing.T) {
	started, gate := make(chan struct{}), make(chan struct{})
	s := newTestSelector(t, loopbackOutbound(t, "a", started, gate), loopbackOutbound(t, "b", nil, nil))
	result := make(chan chan error, 1)
	go func() { result <- routeFlow(t, s) }()

	<-started // the dial to "a" is in flight when the user switches
	require.True(t, s.SelectOutbound("b"))
	close(gate)
	requireClosed(t, <-result)
}

func TestMutableSelector_ReselectClosesAsyncHandlerFlowDialedLate(t *testing.T) {
	started, gate := make(chan struct{}), make(chan struct{})
	s := newTestSelector(t, loopbackOutbound(t, "b", nil, nil))
	async := &handlerOutbound{mockOutbound: loopbackOutbound(t, "async", started, gate), connMgr: s.connMgr, async: true}
	s.outbounds["async"] = async
	s.tags = append(s.tags, "async")
	require.True(t, s.SelectOutbound("async"))

	closed := routeFlow(t, s) // returns before the handler's goroutine dials
	<-started
	require.True(t, s.SelectOutbound("b"))
	close(gate)
	requireClosed(t, closed)
}

func TestMutableSelector_ReselectClosesFlowsOfNestedHandler(t *testing.T) {
	s := newTestSelector(t, loopbackOutbound(t, "b", nil, nil))
	nested := &handlerOutbound{mockOutbound: loopbackOutbound(t, "auto", nil, nil), connMgr: s.connMgr}
	s.outbounds["auto"] = nested
	s.tags = append(s.tags, "auto")
	require.True(t, s.SelectOutbound("auto"))
	closed := routeFlow(t, s)

	require.True(t, s.SelectOutbound("b"))
	requireClosed(t, closed)
}

func TestMutableSelector_RemovingLastOutboundClosesFlows(t *testing.T) {
	s := newTestSelector(t, loopbackOutbound(t, "a", nil, nil))
	closed := routeFlow(t, s)
	dialed, err := s.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("example.com:443"))
	require.NoError(t, err)

	n, err := s.Remove("a")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	requireClosed(t, closed)
	require.Eventually(t, func() bool {
		_ = dialed.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, err := dialed.Read(make([]byte, 1))
		return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
	}, 5*time.Second, 20*time.Millisecond, "conns dialed through the selector must close too")
}

func TestMutableSelector_ReselectDuringDialContextClosesConn(t *testing.T) {
	started, gate := make(chan struct{}), make(chan struct{})
	s := newTestSelector(t, loopbackOutbound(t, "a", started, gate), loopbackOutbound(t, "b", nil, nil))
	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult, 1)
	go func() {
		conn, err := s.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("example.com:443"))
		result <- dialResult{conn, err}
	}()

	<-started
	require.True(t, s.SelectOutbound("b"))
	close(gate)
	r := <-result
	require.ErrorIs(t, r.err, errSelectionChanged, "a dial that finishes after the switch must fail, not return a closing conn")
	require.Nil(t, r.conn)
}

func TestMutableSelector_AddAfterEmptySelects(t *testing.T) {
	a, b := loopbackOutbound(t, "a", nil, nil), loopbackOutbound(t, "b", nil, nil)
	s := newTestSelector(t, a)
	s.outboundMgr = &mockOutboundManager{outbounds: map[string]A.Outbound{"b": b}}
	_, err := s.Remove("a")
	require.NoError(t, err)
	require.Empty(t, s.Now())

	n, err := s.Add("b")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, "b", s.Now(), "adding to an empty group must select")
	requireOpen(t, routeFlow(t, s))
}

func TestMutableSelector_StartSelectsFirstOutbound(t *testing.T) {
	a, b := loopbackOutbound(t, "a", nil, nil), loopbackOutbound(t, "b", nil, nil)
	s := &MutableSelector{
		Adapter:     outbound.NewAdapter(constant.TypeMutableSelector, "manual", nil, nil),
		ctx:         context.Background(),
		outboundMgr: &mockOutboundManager{outbounds: map[string]A.Outbound{"a": a, "b": b}},
		connMgr:     route.NewConnectionManager(log.NewNOPFactory().Logger()),
		logger:      log.NewNOPFactory().Logger(),
		tags:        []string{"a", "b"},
		outbounds:   make(map[string]A.Outbound),
	}
	require.NoError(t, s.Start())
	require.Equal(t, "a", s.Now())
	closed := routeFlow(t, s)
	requireOpen(t, closed)

	require.True(t, s.SelectOutbound("b"))
	requireClosed(t, closed)
}

func TestMutableSelector_SwitchRetiresOldSelectionBeforeReturning(t *testing.T) {
	s := newTestSelector(t, loopbackOutbound(t, "a", nil, nil), loopbackOutbound(t, "b", nil, nil))
	old := s.selected.Load()
	require.True(t, s.SelectOutbound("b"))
	require.True(t, old.group.Retired(), "a dial finishing right after the switch must see it")

	old = s.selected.Load()
	_, err := s.Remove("a", "b")
	require.NoError(t, err)
	require.True(t, old.group.Retired())
}
