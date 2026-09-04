package twiddle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
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
			MasqueradeUpstream: "www.microsoft.com:443",
			TicketMaxAge:       "24h",
		})
	if err != nil {
		t.Fatal(err)
	}
	if ib == nil {
		t.Fatal("nil inbound")
	}
	if ib.(*Inbound).cfg.Cover.Host != "www.microsoft.com" {
		t.Errorf("cover is %q", ib.(*Inbound).cfg.Cover.Host)
	}
	if ib.(*Inbound).cfg.Cover.BinderLen != 48 {
		t.Errorf("microsoft binder %d, want 48", ib.(*Inbound).cfg.Cover.BinderLen)
	}
}

func TestInboundAppliesDefaultTicketMaxAge(t *testing.T) {
	keyHex, _, _ := creds(t)
	ib, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{
			TicketKey: keyHex, MasqueradeUpstream: "www.cloudflare.com:443",
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := ib.(*Inbound).cfg.MaxAge; got != tw.DefaultTicketMaxAge {
		t.Fatalf("default ticket max age is %v, want %v", got, tw.DefaultTicketMaxAge)
	}
}

func TestInboundRejectsUnknownCover(t *testing.T) {
	keyHex, _, _ := creds(t)
	_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{
			TicketKey: keyHex, MasqueradeUpstream: "www.example.com:443",
		})
	if err == nil {
		t.Fatal("inbound accepted an unmeasured cover")
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
			Ticket:        ticket, PSK: psk, CoverSNI: "www.cloudflare.com",
		})
	if err != nil {
		t.Fatal(err)
	}
	o := ob.(*Outbound)
	if len(o.cfg.Pool) == 0 {
		t.Fatal("outbound has an empty hello pool")
	}
	if o.cfg.Cover.Host != "www.cloudflare.com" {
		t.Errorf("cover is %q", o.cfg.Cover.Host)
	}
}

func TestOutboundRejectsUnknownCover(t *testing.T) {
	_, ticket, psk := creds(t)
	_, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "www.example.com",
		})
	if err == nil {
		t.Fatal("outbound accepted an unmeasured cover")
	}
}

// Ticket length is a fidelity parameter of the cover, so a credential minted for
// one identity cannot be presented as another: the hello would be the wrong size
// for the SNI it carries.
func TestOutboundRejectsCredentialForAnotherCover(t *testing.T) {
	_, ticket, psk := creds(t)
	_, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "www.microsoft.com",
		})
	if err == nil {
		t.Fatal("outbound accepted a credential with the wrong ticket length for its cover")
	}
}

// A corrupt configured pool must DEGRADE to the built-in one, not fail the
// outbound.
//
// This reverses the earlier contract deliberately. A pool arrives by config
// push, so a bad one reaches every client at once; the two candidate failure
// modes are "every client emits a stale fingerprint" and "every client has no
// outbound at all". The first risks detection, probabilistically and
// recoverably; the second is a certain outage. Both are fixed by pushing new
// config, so the interim state is the whole of the difference, and staleness is
// the better interim state.
//
// The compensating requirement is that the degradation be visible, which is
// what poolOrigin and the logged Skipped errors are for.
func TestOutboundDegradesToEmbeddedOnACorruptPool(t *testing.T) {
	_, ticket, psk := creds(t)
	ob, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "www.cloudflare.com",
			HelloPool: "not hex at all",
		})
	if err != nil {
		t.Fatalf("a corrupt pool must not brick the outbound: %v", err)
	}
	o := ob.(*Outbound)
	if o.poolOrigin != tw.OriginEmbedded {
		t.Errorf("pool origin is %v, want embedded", o.poolOrigin)
	}
	if len(o.cfg.Pool) == 0 {
		t.Error("degraded outbound has no hellos at all")
	}
}

// The device tap outranks both config tiers, and an inline config pool outranks
// the built-in one. This is the precedence the whole three-tier arrangement
// exists for, so it is asserted here and not just in the twiddle module.
func TestOutboundPoolPrecedence(t *testing.T) {
	_, ticket, psk := creds(t)

	// One real hello, rewritten so each tier is distinguishable by SNI.
	poolFor := func(t *testing.T, name string) string {
		t.Helper()
		h, err := tw.ParseClientHello(tw.DefaultPool()[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := h.SetSNI(name); err != nil {
			t.Fatal(err)
		}
		return tw.FormatPool([][]byte{h.Marshal()})
	}
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "pool.hex")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	devicePath := write(t, poolFor(t, "device.example"))
	configPath := write(t, poolFor(t, "configfile.example"))
	inline := poolFor(t, "inline.example")

	for _, tc := range []struct {
		name       string
		opts       option.TwiddleOutboundOptions
		wantOrigin tw.Origin
		wantSNI    string
	}{
		{
			name:       "device beats everything",
			opts:       option.TwiddleOutboundOptions{HelloPoolDevicePath: devicePath, HelloPoolPath: configPath, HelloPool: inline},
			wantOrigin: tw.OriginDevice, wantSNI: "device.example",
		},
		{
			name:       "config file beats inline",
			opts:       option.TwiddleOutboundOptions{HelloPoolPath: configPath, HelloPool: inline},
			wantOrigin: tw.OriginConfig, wantSNI: "configfile.example",
		},
		{
			name:       "inline beats embedded",
			opts:       option.TwiddleOutboundOptions{HelloPool: inline},
			wantOrigin: tw.OriginConfig, wantSNI: "inline.example",
		},
		{
			name:       "nothing configured falls back",
			opts:       option.TwiddleOutboundOptions{},
			wantOrigin: tw.OriginEmbedded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.opts
			o.ServerOptions = boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443}
			o.Ticket, o.PSK, o.CoverSNI = ticket, psk, "www.cloudflare.com"
			ob, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t", o)
			if err != nil {
				t.Fatal(err)
			}
			out := ob.(*Outbound)
			if out.poolOrigin != tc.wantOrigin {
				t.Errorf("origin = %v, want %v", out.poolOrigin, tc.wantOrigin)
			}
			if tc.wantSNI == "" {
				return
			}
			h, err := tw.ParseClientHello(out.cfg.Pool[0])
			if err != nil {
				t.Fatal(err)
			}
			if h.SNI() != tc.wantSNI {
				t.Errorf("loaded the wrong tier: SNI %q, want %q", h.SNI(), tc.wantSNI)
			}
		})
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
	cover, err := tw.CoverFor("www.cloudflare.com")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := key.Issue(1, cover.TicketLen)
	if err != nil {
		t.Fatal(err)
	}
	// One gate for the whole egress: tickets are single-use, so a per-connection
	// cache would spend nothing.
	replay := tw.NewReplayCache(64, 0)

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
				tc, err := tw.Server(c, tw.ServerConfig{
					TicketKey: key, Cover: cover, Replay: replay,
				})
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
			CoverSNI:      cover.Host,
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
	// > 1, not > 0: takeCredential deliberately never removes the final
	// credential, so len(creds) can never reach zero and an "is it empty"
	// assertion holds even if putCredential is never called at all. Growth
	// past the single seeded credential is the thing worth asserting -- it can
	// only happen if the egress's issued credential came back in the flight and
	// was stored.
	if pooled <= 1 {
		t.Errorf("credential pool did not grow past the seeded credential (%d) after %d successful openings; rotation is not being stored", pooled, ok)
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

// TestCoverProfileReachesServerConfig covers the dead option review found. The
// individual knobs it replaced could name one identity and emit another's
// parameters, so what is pinned now is that the whole measured profile arrives.
func TestCoverProfileReachesServerConfig(t *testing.T) {
	keyHex, _, _ := creds(t)
	ib, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{
			TicketKey: keyHex, MasqueradeUpstream: "www.google.com:443",
		})
	if err != nil {
		t.Fatal(err)
	}
	got := ib.(*Inbound).cfg.Cover
	if !got.PSKFirst || got.TicketLen != 230 || got.BinderLen != 32 {
		t.Errorf("google profile not applied: %+v", got)
	}
}
