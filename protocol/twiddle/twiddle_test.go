package twiddle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sagernet/sing-box/log"
	boxoption "github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/getlantern/lantern-box/option"
	tw "github.com/getlantern/twiddle"
)

func creds(t *testing.T) (keyHex, ticketB64, pskHex string) {
	t.Helper()
	k, err := tw.NewTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := k.Issue(1, tw.DefaultTicketLen)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(c.Ticket), hex.EncodeToString(c.PSK[:])
}

func TestInboundRequiresMasqueradeUpstream(t *testing.T) {
	keyHex, _, _ := creds(t)
	_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{TicketKey: keyHex})
	if err == nil {
		t.Fatal("inbound accepted a config with no masquerade_upstream")
	}
	// twiddle cannot complete a real handshake with an unrecognised peer, so an
	// egress without a cover site hands probes a distinguishing reply.
	if !strings.Contains(err.Error(), "masquerade_upstream") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInboundRejectsBadTicketKey(t *testing.T) {
	for _, k := range []string{"", "zz", hex.EncodeToString(make([]byte, 16))} {
		_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
			option.TwiddleInboundOptions{TicketKey: k, MasqueradeUpstream: "www.example.com:443"})
		if err == nil {
			t.Errorf("inbound accepted ticket_key %q", k)
		}
	}
}

func TestInboundAcceptsAValidConfig(t *testing.T) {
	keyHex, _, _ := creds(t)
	ib, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{
			TicketKey:          keyHex,
			MasqueradeUpstream: "www.example.com:443",
			TicketMaxAge:       "24h",
			TicketLen:          256,
		})
	if err != nil {
		t.Fatal(err)
	}
	if ib == nil {
		t.Fatal("nil inbound")
	}
}

func TestOutboundRequiresCoverSNI(t *testing.T) {
	_, ticket, psk := creds(t)
	_, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk,
		})
	if err == nil {
		t.Fatal("outbound accepted a config with no cover_sni")
	}
	if !strings.Contains(err.Error(), "cover_sni") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOutboundRejectsBadCredentials(t *testing.T) {
	_, ticket, psk := creds(t)
	bad := []option.TwiddleOutboundOptions{
		{Ticket: "not base64!!", PSK: psk, CoverSNI: "a.example"},
		{Ticket: ticket, PSK: "zz", CoverSNI: "a.example"},
		{Ticket: ticket, PSK: hex.EncodeToString(make([]byte, 16)), CoverSNI: "a.example"},
	}
	for i, o := range bad {
		o.ServerOptions = boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443}
		if _, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t", o); err == nil {
			t.Errorf("case %d: outbound accepted bad credentials", i)
		}
	}
}

func TestOutboundUsesTheEmbeddedPoolByDefault(t *testing.T) {
	_, ticket, psk := creds(t)
	ob, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "www.microsoft.com",
		})
	if err != nil {
		t.Fatal(err)
	}
	o := ob.(*Outbound)
	if len(o.cfg.Pool) == 0 {
		t.Fatal("outbound has an empty hello pool")
	}
	if o.cfg.CoverSNI != "www.microsoft.com" {
		t.Errorf("cover SNI is %q", o.cfg.CoverSNI)
	}
}

func TestOutboundRejectsACorruptPool(t *testing.T) {
	_, ticket, psk := creds(t)
	_, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "a.example",
			HelloPool: "not hex at all",
		})
	if err == nil {
		t.Fatal("outbound accepted a corrupt hello pool")
	}
}

// TestPeekConnStopsAndIsBounded covers the leak review found: peekConn recorded
// every byte read and never stopped, so an authenticated session buffered its
// entire lifetime's traffic. It must stop once authentication resolves, and stay
// bounded before that, since an unauthenticated peer chooses how much it sends.
func TestPeekConnStopsAndIsBounded(t *testing.T) {
	server, client := net.Pipe()
	pc := &peekConn{Conn: server}

	go func() {
		buf := make([]byte, 4096)
		for i := 0; i < 40; i++ {
			client.Write(buf)
		}
		client.Close()
	}()

	buf := make([]byte, 4096)
	for i := 0; i < 20; i++ {
		if _, err := pc.Read(buf); err != nil {
			break
		}
	}
	if got := len(pc.replay()); got > maxPeek {
		t.Errorf("replay buffer grew to %d, past the %d cap", got, maxPeek)
	}

	// after stop() nothing more is recorded, however much arrives
	pc.stop()
	before := len(pc.replay())
	for i := 0; i < 20; i++ {
		if _, err := pc.Read(buf); err != nil {
			break
		}
	}
	if after := len(pc.replay()); after != before {
		t.Errorf("recording continued after stop(): %d -> %d", before, after)
	}
	server.Close()
}

// TestPeekConnReplayIsFaithful: what a prober sent must reach the cover site
// intact, so the recorded prefix has to match the bytes byte for byte.
func TestPeekConnReplayIsFaithful(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	pc := &peekConn{Conn: server}
	want := []byte("\x16\x03\x01\x00\x05hello-prober")
	go func() { client.Write(want); client.Close() }()

	buf := make([]byte, 64)
	n, _ := pc.Read(buf)
	if got := pc.replay(); string(got) != string(want[:n]) {
		t.Errorf("replay is %q, want %q", got, want[:n])
	}
}

// TestConcurrentDialsExerciseTheCredentialPath replaces an earlier version that
// pointed at port 1. Review caught that the TCP dial fails before DialContext
// ever reaches the credential code, so the test asserted nothing about the path
// it named -- a vacuous test that would have passed with the pool removed.
//
// This one stands up a real twiddle egress, so every dial completes an opening
// and actually claims and returns a credential. Run under -race.
func TestConcurrentDialsExerciseTheCredentialPath(t *testing.T) {
	key, err := tw.NewTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := key.Issue(1, tw.DefaultTicketLen)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var served int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc, err := tw.Server(c, tw.ServerConfig{TicketKey: key})
				if err != nil {
					return
				}
				if _, err := readDestination(tc); err == nil {
					atomic.AddInt64(&served, 1)
				}
			}(c)
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ob, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: host, ServerPort: uint16(port)},
			Ticket:        base64.StdEncoding.EncodeToString(cred.Ticket),
			PSK:           hex.EncodeToString(cred.PSK[:]),
			CoverSNI:      "www.example.com",
		})
	if err != nil {
		t.Fatal(err)
	}
	o := ob.(*Outbound)

	const n = 24
	var wg sync.WaitGroup
	var ok int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := o.DialContext(context.Background(), "tcp", M.ParseSocksaddr("example.com:443"))
			if err == nil {
				atomic.AddInt64(&ok, 1)
				c.Close()
			}
		}()
	}
	wg.Wait()

	if ok == 0 {
		t.Fatal("no dial completed — the credential path was never exercised")
	}
	o.mu.Lock()
	pooled := len(o.creds)
	o.mu.Unlock()
	if pooled == 0 {
		t.Error("credential pool is empty; a dial consumed the last credential")
	}
	t.Logf("%d/%d dials completed, %d openings served, %d credentials pooled",
		ok, n, atomic.LoadInt64(&served), pooled)
}

// TestCredentialPoolHandsOutDistinctCredentials: sequential connections must
// never present the same ticket twice, which is what the pool is for.
func TestCredentialPoolHandsOutDistinctCredentials(t *testing.T) {
	key, _ := tw.NewTicketKey()
	c1, _ := key.Issue(1, tw.DefaultTicketLen)
	c2, _ := key.Issue(1, tw.DefaultTicketLen)
	o := &Outbound{creds: []*tw.Credential{c1, c2}}

	a := o.takeCredential()
	b := o.takeCredential()
	if bytes.Equal(a.Ticket, b.Ticket) {
		t.Error("two takes returned the same credential while the pool held two")
	}
	// the last credential is reused rather than removed, so a dial never starves
	c := o.takeCredential()
	if c == nil {
		t.Fatal("takeCredential returned nil on an exhausted pool")
	}
	o.putCredential(c1)
	o.mu.Lock()
	got := len(o.creds)
	o.mu.Unlock()
	if got != 2 {
		t.Errorf("pool has %d credentials after a put, want 2", got)
	}
}

// TestCredentialPoolIsBounded guards against unbounded growth on a long-lived
// outbound.
func TestCredentialPoolIsBounded(t *testing.T) {
	key, _ := tw.NewTicketKey()
	seed, _ := key.Issue(1, tw.DefaultTicketLen)
	o := &Outbound{creds: []*tw.Credential{seed}}
	for i := 0; i < maxCredPool*3; i++ {
		c, _ := key.Issue(1, tw.DefaultTicketLen)
		o.putCredential(c)
	}
	o.mu.Lock()
	got := len(o.creds)
	o.mu.Unlock()
	if got > maxCredPool {
		t.Errorf("pool grew to %d, past the %d cap", got, maxCredPool)
	}
}

// TestPSKFirstReachesServerConfig covers the dead option review found.
func TestPSKFirstReachesServerConfig(t *testing.T) {
	keyHex, _, _ := creds(t)
	for _, want := range []bool{false, true} {
		ib, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
			option.TwiddleInboundOptions{
				TicketKey: keyHex, MasqueradeUpstream: "www.example.com:443", PSKFirst: want,
			})
		if err != nil {
			t.Fatal(err)
		}
		if got := ib.(*Inbound).cfg.PSKFirst; got != want {
			t.Errorf("psk_first=%v did not reach ServerConfig (got %v)", want, got)
		}
	}
}
