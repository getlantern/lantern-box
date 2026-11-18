package outline

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/metadata"
	"golang.getoutline.org/sdk/transport"
	"golang.getoutline.org/sdk/x/configurl"
	"golang.getoutline.org/sdk/x/smart"

	"github.com/getlantern/lantern-box/option"
)

var errDialRefused = errors.New("dial refused")

type recordingDialer struct {
	mu    sync.Mutex
	addrs []string
}

func (d *recordingDialer) record(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, addr)
}

func (d *recordingDialer) DialStream(_ context.Context, addr string) (transport.StreamConn, error) {
	d.record(addr)
	return nil, errDialRefused
}

func (d *recordingDialer) DialPacket(_ context.Context, addr string) (net.Conn, error) {
	d.record(addr)
	return nil, errDialRefused
}

func (d *recordingDialer) dialed(addr string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.addrs, addr)
}

func TestSmartConfigAcceptedByFinder(t *testing.T) {
	options := option.OutboundOutlineOptions{
		DialerOptions: O.DialerOptions{Detour: "direct", AbstractDialerOptions: O.AbstractDialerOptions{BindInterface: "eth0"}},
		ServerOptions: O.ServerOptions{Server: "example.com", ServerPort: 443},
		DNSResolvers: []option.DNSEntryConfig{
			{HTTPS: &option.HTTPSEntryConfig{Name: "cloudflare-dns.com", Address: "192.0.2.1:443"}},
			{TLS: &option.TLSEntryConfig{Name: "dns.google", Address: "192.0.2.2:853"}},
			{TCP: &option.TCPEntryConfig{Address: "192.0.2.3:53"}},
			{UDP: &option.UDPEntryConfig{Address: "192.0.2.4:53"}},
		},
		TLS:         []string{"", "split:1", "tlsfrag:1"},
		Fallback:    []string{"socks5://192.0.2.5:1080"},
		TestTimeout: "5s",
		Domains:     []string{"cdn.jsdelivr.net"},
	}
	config, err := marshalSmartConfig(options)
	if err != nil {
		t.Fatalf("marshalSmartConfig: %v", err)
	}

	d := &recordingDialer{}
	registry := configurl.NewDefaultRegistry()
	configurl.RegisterBase(registry, func(context.Context, configurl.Registry, *configurl.Config) (transport.StreamDialer, error) {
		return d, nil
	})
	configurl.RegisterBase(registry, func(context.Context, configurl.Registry, *configurl.Config) (transport.PacketDialer, error) {
		return d, nil
	})
	finder, err := smart.NewFinderConfig().SetTestTimeout(5 * time.Second).SetConfigURLRegistry(registry).New()
	if err != nil {
		t.Fatalf("FinderConfig.New: %v", err)
	}
	_, err = finder.NewDialer(context.Background(), options.Domains, config)
	if err == nil {
		t.Fatal("NewDialer succeeded with every dial refused")
	}
	if strings.Contains(err.Error(), "parse config") {
		t.Fatalf("Finder rejected config:\n%s\nerror: %v", config, err)
	}
	for _, addr := range []string{"192.0.2.1:443", "192.0.2.2:853", "192.0.2.3:53", "192.0.2.4:53", "192.0.2.5:1080"} {
		if !d.dialed(addr) {
			t.Errorf("resolver %s was never tried; config:\n%s", addr, config)
		}
	}
}

func TestDialContextHost(t *testing.T) {
	addr := metadata.ParseSocksaddr
	for _, tc := range []struct {
		name string
		meta *adapter.InboundContext // inbound metadata, nil for none
		dest metadata.Socksaddr
		want string
	}{
		// A remote rule-set's download_detour dials the outbound directly.
		{"no metadata, hostname destination", nil, addr("cdn.jsdelivr.net:443"), "cdn.jsdelivr.net:443"},
		{"no metadata, IPv4 destination", nil, addr("192.0.2.1:443"), "192.0.2.1:443"},
		{"no metadata, IPv6 destination", nil, addr("[2001:db8::1]:443"), "[2001:db8::1]:443"},
		{"no metadata, IPv4-mapped destination", nil,
			metadata.Socksaddr{Addr: netip.MustParseAddr("::ffff:192.0.2.1"), Port: 443}, "192.0.2.1:443"},
		{"routed with sniffed domain", &adapter.InboundContext{Domain: "example.com"}, addr("192.0.2.1:443"), "example.com:443"},
		{"routed without sniffed domain", &adapter.InboundContext{}, addr("192.0.2.1:443"), "192.0.2.1:443"},
		{"inherited domain doesn't override a hostname destination",
			&adapter.InboundContext{Domain: "youtube.com"}, addr("dns.google:853"), "dns.google:853"},
		{"hostname kept when the destination also has an IP", nil,
			metadata.Socksaddr{Addr: netip.MustParseAddr("192.0.2.1"), Fqdn: "cdn.jsdelivr.net", Port: 443}, "cdn.jsdelivr.net:443"},
		{"non-strict hostname still dialed", &adapter.InboundContext{Domain: "youtube.com"},
			metadata.Socksaddr{Fqdn: "bücher.de", Port: 443}, "bücher.de:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &recordingDialer{}
			o := &Outbound{createDialer: func() (transport.StreamDialer, error) { return d, nil }}
			ctx := context.Background()
			if tc.meta != nil {
				ctx = adapter.WithContext(ctx, tc.meta)
			}
			if _, err := o.DialContext(ctx, "tcp", tc.dest); !errors.Is(err, errDialRefused) {
				t.Fatalf("DialContext: got %v, want %v", err, errDialRefused)
			}
			if !d.dialed(tc.want) {
				t.Fatalf("dialed %v, want %q", d.addrs, tc.want)
			}
		})
	}
}

func TestDialContextNoHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta *adapter.InboundContext // inbound metadata, nil for none
	}{
		{"no metadata", nil},
		// An inherited domain must not stand in for a missing destination.
		{"inherited domain", &adapter.InboundContext{Domain: "example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var searches int
			o := &Outbound{createDialer: func() (transport.StreamDialer, error) {
				searches++
				return &recordingDialer{}, nil
			}}
			ctx := context.Background()
			if tc.meta != nil {
				ctx = adapter.WithContext(ctx, tc.meta)
			}
			_, err := o.DialContext(ctx, "tcp", metadata.Socksaddr{Port: 443})
			if err == nil || errors.Is(err, errDialRefused) {
				t.Fatalf("DialContext: got %v, want a no-host error before dialing", err)
			}
			if searches != 0 {
				t.Fatalf("ran %d strategy searches for a destination with no host", searches)
			}
		})
	}
}

type failingStreamDialer struct{ err error }

func (d failingStreamDialer) DialStream(context.Context, string) (transport.StreamConn, error) {
	return nil, d.err
}

func TestDialContextRetriesFailedDialerCreation(t *testing.T) {
	errSearch := errors.New("no working strategy")
	errStream := errors.New("stream dial failed")
	var calls int
	o := &Outbound{
		createDialer: func() (transport.StreamDialer, error) {
			calls++
			if calls == 1 {
				return nil, errSearch
			}
			return failingStreamDialer{err: errStream}, nil
		},
	}
	dest := metadata.ParseSocksaddr("example.com:443")

	if _, err := o.DialContext(context.Background(), "tcp", dest); !errors.Is(err, errSearch) {
		t.Fatalf("first dial: got %v, want %v", err, errSearch)
	}
	for range 2 {
		if _, err := o.DialContext(context.Background(), "tcp", dest); !errors.Is(err, errStream) {
			t.Fatalf("dial after failed search: got %v, want %v", err, errStream)
		}
	}
	if calls != 2 {
		t.Fatalf("createDialer called %d times, want 2", calls)
	}
}

func TestConcurrentDialsShareOneSearch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errSearch := errors.New("no working strategy")
		release := make(chan struct{})
		var calls atomic.Int32
		o := &Outbound{
			createDialer: func() (transport.StreamDialer, error) {
				calls.Add(1)
				<-release
				return nil, errSearch
			},
		}
		dest := metadata.ParseSocksaddr("example.com:443")

		const dials = 5
		errs := make(chan error, dials)
		for range dials {
			go func() {
				_, err := o.DialContext(context.Background(), "tcp", dest)
				errs <- err
			}()
		}
		synctest.Wait()
		close(release)
		for range dials {
			if err := <-errs; !errors.Is(err, errSearch) {
				t.Fatalf("concurrent dial: got %v, want %v", err, errSearch)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("createDialer called %d times for concurrent dials, want 1", got)
		}

		if _, err := o.DialContext(context.Background(), "tcp", dest); !errors.Is(err, errSearch) {
			t.Fatalf("dial after shared failure: got %v, want %v", err, errSearch)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("createDialer called %d times after a later dial, want 2", got)
		}
	})
}

func TestCancelledDialLeavesSearchRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errStream := errors.New("stream dial failed")
		release := make(chan struct{})
		var calls atomic.Int32
		o := &Outbound{
			createDialer: func() (transport.StreamDialer, error) {
				calls.Add(1)
				<-release
				return failingStreamDialer{err: errStream}, nil
			},
		}
		dest := metadata.ParseSocksaddr("example.com:443")

		ctx, cancel := context.WithCancel(context.Background())
		errs := make(chan error, 1)
		go func() {
			_, err := o.DialContext(ctx, "tcp", dest)
			errs <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled dial: got %v, want %v", err, context.Canceled)
		}

		close(release)
		synctest.Wait()
		if _, err := o.DialContext(context.Background(), "tcp", dest); !errors.Is(err, errStream) {
			t.Fatalf("dial after search finished: got %v, want %v", err, errStream)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("createDialer called %d times, want 1", got)
		}
	})
}

func TestNewOutboundOptions(t *testing.T) {
	ptr := func(s string) *string { return &s }
	for _, tc := range []struct {
		name     string
		mutate   func(*option.OutboundOutlineOptions)
		networks []string
		wantErr  string
	}{
		{"defaults are TCP-only", func(*option.OutboundOutlineOptions) {}, []string{"tcp"}, ""},
		{"zero timeout uses the SDK default", func(o *option.OutboundOutlineOptions) { o.TestTimeout = "0s" }, []string{"tcp"}, ""},
		{"bad timeout", func(o *option.OutboundOutlineOptions) { o.TestTimeout = "soon" }, nil, "soon"},
		{"plain UDP", func(o *option.OutboundOutlineOptions) { o.UDP = ptr("") }, []string{"tcp", "udp"}, ""},
		{"quicprelude UDP", func(o *option.OutboundOutlineOptions) { o.UDP = ptr("quicprelude:count=2&version=reserved") }, []string{"tcp", "udp"}, ""},
		{"bad quicprelude option", func(o *option.OutboundOutlineOptions) { o.UDP = ptr("quicprelude:mode=nonsense") }, nil, "nonsense"},
		{"stream-only transport rejected for UDP", func(o *option.OutboundOutlineOptions) { o.UDP = ptr("tlsfrag:1") }, nil, "udp config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := option.OutboundOutlineOptions{Domains: []string{"example.com"}}
			tc.mutate(&options)
			out, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "outline", options)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NewOutbound: got %v, want an error mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewOutbound: %v", err)
			}
			if got := out.Network(); !slices.Equal(got, tc.networks) {
				t.Fatalf("Network() = %v, want %v", got, tc.networks)
			}
			_, err = out.ListenPacket(context.Background(), metadata.ParseSocksaddr("127.0.0.1:443"))
			if udp := slices.Contains(tc.networks, "udp"); !udp && err == nil {
				t.Fatal("ListenPacket succeeded on a TCP-only outbound")
			}
		})
	}
}

func TestDialContextUDPRoundTrip(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(buf[:n], from)
		}
	}()
	for _, udp := range []string{"", "quicprelude:count=1"} {
		t.Run(udp, func(t *testing.T) {
			out, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "outline",
				option.OutboundOutlineOptions{Domains: []string{"example.com"}, UDP: &udp})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := out.DialContext(context.Background(), "udp", metadata.SocksaddrFromNet(echo.LocalAddr()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatalf("Write: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil || string(buf[:n]) != "ping" {
				t.Fatalf("Read = %q, %v", buf[:n], err)
			}
		})
	}
}
