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
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/metadata"
	"golang.getoutline.org/sdk/transport"
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

func TestSmartConfigAcceptedByStrategyFinder(t *testing.T) {
	options := option.OutboundOutlineOptions{
		DialerOptions: O.DialerOptions{Detour: "direct", BindInterface: "eth0"},
		ServerOptions: O.ServerOptions{Server: "example.com", ServerPort: 443},
		DNSResolvers: []option.DNSEntryConfig{
			{HTTPS: &option.HTTPSEntryConfig{Name: "cloudflare-dns.com", Address: "192.0.2.1:443"}},
			{TLS: &option.TLSEntryConfig{Name: "dns.google", Address: "192.0.2.2:853"}},
			{TCP: &option.TCPEntryConfig{Address: "192.0.2.3:53"}},
			{UDP: &option.UDPEntryConfig{Address: "192.0.2.4:53"}},
		},
		TLS:         []string{"", "split:1", "tlsfrag:1"},
		TestTimeout: "5s",
		Domains:     []string{"cdn.jsdelivr.net"},
	}
	config, err := marshalSmartConfig(options)
	if err != nil {
		t.Fatalf("marshalSmartConfig: %v", err)
	}

	d := &recordingDialer{}
	finder := &smart.StrategyFinder{TestTimeout: 5 * time.Second, StreamDialer: d, PacketDialer: d}
	_, err = finder.NewDialer(context.Background(), options.Domains, config)
	if err == nil {
		t.Fatal("NewDialer succeeded with every dial refused")
	}
	if strings.Contains(err.Error(), "parse config") {
		t.Fatalf("StrategyFinder rejected config:\n%s\nerror: %v", config, err)
	}
	for _, addr := range []string{"192.0.2.1:443", "192.0.2.2:853", "192.0.2.3:53", "192.0.2.4:53"} {
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
