package outline

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
