package twiddle

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net"
	"strings"
	"sync"
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

// TestConcurrentDialsDoNotRaceOnCredential covers the data race review found:
// DialContext rotated o.cfg.Credential in place, and sing-box dials
// concurrently. Run with -race.
func TestConcurrentDialsDoNotRaceOnCredential(t *testing.T) {
	_, ticket, psk := creds(t)
	ob, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
			Ticket:        ticket, PSK: psk, CoverSNI: "www.example.com",
		})
	if err != nil {
		t.Fatal(err)
	}
	o := ob.(*Outbound)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// dials fail (port 1), but the credential read/rotate path still runs
			o.DialContext(context.Background(), "tcp", M.ParseSocksaddr("example.com:443"))
			o.mu.Lock()
			_ = o.cred
			o.mu.Unlock()
		}()
	}
	wg.Wait()
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
