package outline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"golang.getoutline.org/sdk/transport"

	"github.com/getlantern/lantern-box/option"
)

func TestSiteOf(t *testing.T) {
	for host, want := range map[string]string{
		"www.youtube.com":  "youtube.com",
		"m.youtube.com":    "youtube.com",
		"youtube.com":      "youtube.com",
		"WWW.YouTube.com.": "youtube.com",
		"news.bbc.co.uk":   "bbc.co.uk",
		// Under a private suffix, each name is its own site.
		"foo.appspot.com": "foo.appspot.com",
		"a.b.github.io":   "b.github.io",
		"appspot.com":     "",
		// Wildcard-only ICANN TLDs, whose bare TLD isn't listed.
		"www.moi.gov.mm":       "moi.gov.mm",
		"kathmandupost.com.np": "kathmandupost.com.np",
		"gov.mm":               "",
		"x.mm":                 "",
		"www.ck":               "www.ck",
		"nas.ck":               "",

		// No registrable domain: IPs, single labels and local names.
		"intranet":          "",
		"192.0.2.1":         "",
		"2001:db8::1":       "",
		"localhost":         "",
		"nas.lan":           "",
		"printer.home.arpa": "",
		"":                  "",
	} {
		if got := siteOf(host); got != want {
			t.Errorf("siteOf(%q) = %q, want %q", host, got, want)
		}
	}
}

// fakeSiteSearch records searches and finishes each with the dialer and error
// the test gives it.
type fakeSiteSearch struct {
	mu      sync.Mutex
	hosts   []string
	release chan struct{} // nil finishes searches at once
	dialer  transport.StreamDialer
	err     error
}

func (f *fakeSiteSearch) search(ctx context.Context, _, host string) (transport.StreamDialer, error) {
	f.mu.Lock()
	f.hosts = append(f.hosts, host)
	release := f.release
	f.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dialer, f.err
}

func (f *fakeSiteSearch) searched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hosts...)
}

func TestSiteDialersSearchInBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &recordingDialer{}
		f := &fakeSiteSearch{release: make(chan struct{}), dialer: d}
		s := newSiteDialers(t.Context(), maxSites, nil, f.search)

		// Dials during the search get no site dialer, and share one search,
		// including a sibling subdomain.
		for _, host := range []string{"www.youtube.com", "www.youtube.com", "m.youtube.com"} {
			if got := s.dialerFor(host, 443); got != nil {
				t.Fatalf("dialerFor(%q) during the search = %v, want nil", host, got)
			}
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 1 || got[0] != "www.youtube.com" {
			t.Fatalf("searched %v, want one search of www.youtube.com", got)
		}

		close(f.release)
		synctest.Wait()
		if got := s.dialerFor("m.youtube.com", 443); got == nil || got.StreamDialer != d {
			t.Fatalf("dialerFor after the search = %v, want the site's dialer", got)
		}
		if got := f.searched(); len(got) != 1 {
			t.Fatalf("searched %v after success, want no further search", got)
		}
	})
}

func TestSiteDialersRetryFailedSearchLater(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSiteSearch{err: errors.New("no strategy")}
		s := newSiteDialers(t.Context(), maxSites, nil, f.search)

		s.dialerFor("www.youtube.com", 443)
		synctest.Wait()
		time.Sleep(siteRetryAfter - time.Second)
		if got := s.dialerFor("www.youtube.com", 443); got != nil {
			t.Fatalf("dialerFor after a failed search = %v, want nil", got)
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 1 {
			t.Fatalf("searched %v before siteRetryAfter, want 1 search", got)
		}

		time.Sleep(time.Second)
		s.dialerFor("www.youtube.com", 443)
		synctest.Wait()
		if got := f.searched(); len(got) != 2 {
			t.Fatalf("searched %v after siteRetryAfter, want 2 searches", got)
		}

		// The second failure in a row doubles the delay.
		time.Sleep(2*siteRetryAfter - time.Second)
		s.dialerFor("www.youtube.com", 443)
		synctest.Wait()
		if got := f.searched(); len(got) != 2 {
			t.Fatalf("searched %v before twice siteRetryAfter, want 2 searches", got)
		}
		time.Sleep(time.Second)
		s.dialerFor("www.youtube.com", 443)
		synctest.Wait()
		if got := f.searched(); len(got) != 3 {
			t.Fatalf("searched %v after twice siteRetryAfter, want 3 searches", got)
		}
	})
}

func TestRetryDelay(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		1: siteRetryAfter, 2: 2 * siteRetryAfter, 3: 4 * siteRetryAfter, 4: 8 * siteRetryAfter, 5: siteRetryMax, 40: siteRetryMax,
	} {
		if got := retryDelay(failures); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", failures, got, want)
		}
	}
}

func TestSiteDialersSearchOnlyFrom443(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &recordingDialer{}
		f := &fakeSiteSearch{dialer: d}
		s := newSiteDialers(t.Context(), maxSites, nil, f.search)
		s.dialerFor("imap.gmail.com", 993)
		synctest.Wait()
		if got := f.searched(); len(got) != 0 {
			t.Fatalf("searched %v from a dial to 993, want none", got)
		}
		s.dialerFor("mail.gmail.com", 443)
		synctest.Wait()
		// Once found, the site's dialer serves its other ports too.
		if got := s.dialerFor("imap.gmail.com", 993); got == nil || got.StreamDialer != d {
			t.Fatalf("dialerFor on 993 after a search = %v, want the site's dialer", got)
		}
	})
}

func TestSiteDialersCapConcurrentSearches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSiteSearch{release: make(chan struct{}), dialer: &recordingDialer{}}
		s := newSiteDialers(t.Context(), maxSites, nil, f.search)
		for i := range maxSiteSearches + 2 {
			s.dialerFor(string(rune('a'+i))+"site.com", 443)
		}
		synctest.Wait()
		if got := f.searched(); len(got) != maxSiteSearches {
			t.Fatalf("%d searches running, want %d", len(got), maxSiteSearches)
		}
		close(f.release)
		synctest.Wait()
		// A site skipped while the slots were full is searched on its next dial.
		s.dialerFor(string(rune('a'+maxSiteSearches))+"site.com", 443)
		synctest.Wait()
		if got := f.searched(); len(got) != maxSiteSearches+1 {
			t.Fatalf("searched %v, want %d searches", got, maxSiteSearches+1)
		}
	})
}

func TestSiteDialersDropFailingDialer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &recordingDialer{}
		f := &fakeSiteSearch{dialer: d}
		s := newSiteDialers(t.Context(), maxSites, nil, f.search)
		s.dialerFor("youtube.com", 443)
		synctest.Wait()
		sd := s.dialerFor("youtube.com", 443)
		if sd == nil || sd.StreamDialer != d {
			t.Fatalf("dialerFor after the search = %v, want the site's dialer", sd)
		}

		// A success resets the count.
		s.dialed("youtube.com", sd, errDialRefused)
		s.dialed("youtube.com", sd, nil)
		for range maxSiteDialFailures - 1 {
			s.dialed("youtube.com", sd, errDialRefused)
		}
		if got := s.dialerFor("youtube.com", 443); got != sd {
			t.Fatalf("dialerFor after %d failures = %v, want the site's dialer", maxSiteDialFailures-1, got)
		}
		s.dialed("youtube.com", sd, errDialRefused)
		if got := s.dialerFor("youtube.com", 443); got != nil {
			t.Fatalf("dialerFor after %d failures = %v, want nil", maxSiteDialFailures, got)
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 1 {
			t.Fatalf("searched %v right after dropping the dialer, want 1 search", got)
		}
		time.Sleep(siteRetryAfter)
		s.dialerFor("youtube.com", 443)
		synctest.Wait()
		if got := f.searched(); len(got) != 2 {
			t.Fatalf("searched %v after siteRetryAfter, want 2 searches", got)
		}
	})
}

func TestSiteDialersSkipHostsWithoutSite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSiteSearch{dialer: &recordingDialer{}}
		s := newSiteDialers(t.Context(), maxSites, nil, f.search)
		for _, host := range []string{"192.0.2.1", "localhost"} {
			if got := s.dialerFor(host, 443); got != nil {
				t.Fatalf("dialerFor(%q) = %v, want nil", host, got)
			}
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 0 {
			t.Fatalf("searched %v, want none", got)
		}
	})
}

func TestSiteDialersEvictLeastRecentSite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSiteSearch{dialer: &recordingDialer{}}
		s := newSiteDialers(t.Context(), 1, nil, f.search)
		s.dialerFor("youtube.com", 443)
		synctest.Wait()
		s.dialerFor("instagram.com", 443)
		synctest.Wait()
		// youtube.com was evicted, so it's searched again.
		if got := s.dialerFor("youtube.com", 443); got != nil {
			t.Fatalf("dialerFor an evicted site = %v, want nil", got)
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 3 {
			t.Fatalf("searched %v, want 3 searches", got)
		}
	})
}

func TestSiteSearchEndsWithOutbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		f := &fakeSiteSearch{release: make(chan struct{}), dialer: &recordingDialer{}}
		s := newSiteDialers(ctx, maxSites, nil, f.search)
		s.dialerFor("youtube.com", 443)
		synctest.Wait()
		cancel()
		synctest.Wait()
		// The cancelled search counts as failed, so the site keeps the
		// outbound's dialer.
		if got := s.dialerFor("youtube.com", 443); got != nil {
			t.Fatalf("dialerFor after the outbound closed = %v, want nil", got)
		}
	})
}

func TestDialContextUsesSiteDialerOnceFound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		outboundDialer, siteDialer := &recordingDialer{}, &recordingDialer{}
		f := &fakeSiteSearch{release: make(chan struct{}), dialer: siteDialer}
		o := &Outbound{
			createDialer: func() (transport.StreamDialer, error) { return outboundDialer, nil },
			sites:        newSiteDialers(t.Context(), maxSites, nil, f.search),
		}
		dial := func(dest string) {
			t.Helper()
			if _, err := o.DialContext(context.Background(), "tcp", metadata.ParseSocksaddr(dest)); !errors.Is(err, errDialRefused) {
				t.Fatalf("DialContext(%s): got %v, want %v", dest, err, errDialRefused)
			}
		}

		dial("www.youtube.com:443")
		if !outboundDialer.dialed("www.youtube.com:443") {
			t.Fatalf("first dial went to %v, want the outbound's dialer", outboundDialer.addrs)
		}

		close(f.release)
		synctest.Wait()
		dial("i.youtube.com:443")
		if !siteDialer.dialed("i.youtube.com:443") {
			t.Fatalf("dial after the search went to %v, want the site's dialer", siteDialer.addrs)
		}
		// The site's dialer failed, so the outbound's dialer got the dial too.
		if !outboundDialer.dialed("i.youtube.com:443") {
			t.Fatalf("dial after the site's dialer failed went to %v, want the outbound's dialer too", outboundDialer.addrs)
		}

		dial("192.0.2.1:443")
		if !outboundDialer.dialed("192.0.2.1:443") {
			t.Fatalf("IP dial went to %v, want the outbound's dialer", outboundDialer.addrs)
		}
	})
}

func TestNewOutboundPerSiteOption(t *testing.T) {
	no, yes := false, true
	doh := option.DNSEntryConfig{HTTPS: &option.HTTPSEntryConfig{Name: "dns.google"}}
	udp := option.DNSEntryConfig{UDP: &option.UDPEntryConfig{Address: "8.8.8.8:53"}}
	for _, tc := range []struct {
		name    string
		perSite *bool
		dns     []option.DNSEntryConfig
		want    bool
	}{
		{"unset", nil, []option.DNSEntryConfig{doh}, true},
		{"true", &yes, []option.DNSEntryConfig{doh}, true},
		{"false", &no, []option.DNSEntryConfig{doh}, false},
		{"plaintext resolvers only", nil, []option.DNSEntryConfig{udp}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := option.OutboundOutlineOptions{Domains: []string{"example.com"}, DNSResolvers: tc.dns, PerSite: tc.perSite}
			out, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "outline", options)
			if err != nil {
				t.Fatalf("NewOutbound: %v", err)
			}
			o := out.(*Outbound)
			if got := o.sites != nil; got != tc.want {
				t.Fatalf("per-site searches on = %v, want %v", got, tc.want)
			}
			if err := o.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if o.sites != nil && o.sites.ctx.Err() == nil {
				t.Fatal("Close left the site searches' context live")
			}
		})
	}
}

func TestPerSiteOptionsKeepOnlyEncryptedResolvers(t *testing.T) {
	doh := option.DNSEntryConfig{HTTPS: &option.HTTPSEntryConfig{Name: "dns.google"}}
	dot := option.DNSEntryConfig{TLS: &option.TLSEntryConfig{Name: "dns.google"}}
	udp := option.DNSEntryConfig{UDP: &option.UDPEntryConfig{Address: "8.8.8.8:53"}}
	mixed := option.DNSEntryConfig{System: &struct{}{}, UDP: udp.UDP, HTTPS: doh.HTTPS}
	in := option.OutboundOutlineOptions{
		DNSResolvers: []option.DNSEntryConfig{udp, doh, dot, mixed},
		Fallback:     []string{"tlsfrag:1"},
	}
	got, ok := perSiteOptions(in)
	if got.Fallback != nil {
		t.Fatalf("perSiteOptions kept fallbacks %v, which resolve with the system resolver", got.Fallback)
	}
	if len(in.Fallback) != 1 {
		t.Fatalf("perSiteOptions changed the outbound's fallbacks to %v", in.Fallback)
	}
	if !ok || len(got.DNSResolvers) != 3 || got.DNSResolvers[0].HTTPS == nil || got.DNSResolvers[1].TLS == nil {
		t.Fatalf("perSiteOptions resolvers = %+v (on %v), want the DoH, DoT and mixed entries", got.DNSResolvers, ok)
	}
	if m := got.DNSResolvers[2]; m.HTTPS == nil || m.System != nil || m.UDP != nil {
		t.Fatalf("mixed entry kept as %+v, want only its DoH resolver", m)
	}
}

// fakeStreamConn records whether it was closed.
type fakeStreamConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *fakeStreamConn) Close() error      { c.closed.Store(true); return nil }
func (c *fakeStreamConn) CloseRead() error  { return nil }
func (c *fakeStreamConn) CloseWrite() error { return nil }

func connDialer(c *fakeStreamConn) transport.StreamDialer {
	return transport.FuncStreamDialer(func(context.Context, string) (transport.StreamConn, error) { return c, nil })
}

// newSiteOutbound returns an outbound whose dialer is outbound, and whose
// sites' searches find site, once dest has been dialed to start one.
func newSiteOutbound(t *testing.T, outbound, site transport.StreamDialer, dest metadata.Socksaddr) *Outbound {
	t.Helper()
	o := &Outbound{
		createDialer: func() (transport.StreamDialer, error) { return outbound, nil },
		sites:        newSiteDialers(t.Context(), maxSites, nil, (&fakeSiteSearch{dialer: site}).search),
	}
	o.DialContext(context.Background(), "tcp", dest)
	synctest.Wait()
	if o.sites.dialerFor(dest.Fqdn, dest.Port) == nil {
		t.Fatal("site search found no dialer")
	}
	return o
}

func TestStalledSiteDialerRacesOutbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var siteDials atomic.Int32
		stalled := transport.FuncStreamDialer(func(ctx context.Context, _ string) (transport.StreamConn, error) {
			siteDials.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		outConn := &fakeStreamConn{}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o := newSiteOutbound(t, connDialer(outConn), stalled, dest)
		for range maxSiteDialFailures {
			start := time.Now()
			conn, err := o.DialContext(context.Background(), "tcp", dest)
			if err != nil || conn != net.Conn(outConn) {
				t.Fatalf("DialContext = %v, %v; want the outbound's connection", conn, err)
			}
			if took := time.Since(start); took != siteDialHeadStart {
				t.Fatalf("dial took %v, want the head start, %v", took, siteDialHeadStart)
			}
		}
		synctest.Wait()
		if got := siteDials.Load(); got != maxSiteDialFailures {
			t.Fatalf("site dialer dialed %d times, want %d", got, maxSiteDialFailures)
		}
		if outConn.closed.Load() {
			t.Fatal("the winning connection was closed")
		}
		if o.sites.dialerFor("www.youtube.com", 443) != nil {
			t.Fatal("a site dialer that lost every race kept its dialer")
		}
	})
}

func TestFastSiteDialerSkipsOutbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		siteConn := &fakeStreamConn{}
		outboundDialer := &recordingDialer{}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o := newSiteOutbound(t, outboundDialer, connDialer(siteConn), dest)
		conn, err := o.DialContext(context.Background(), "tcp", dest)
		if err != nil || conn != net.Conn(siteConn) {
			t.Fatalf("DialContext = %v, %v; want the site's connection", conn, err)
		}
		time.Sleep(2 * siteDialHeadStart)
		synctest.Wait()
		if len(outboundDialer.addrs) != 1 {
			t.Fatalf("outbound's dialer dialed %v, want only the dial that started the search", outboundDialer.addrs)
		}
	})
}

func TestLateSiteConnectionClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		late := &fakeStreamConn{}
		// Ignores its context, as a dialer stuck in a syscall would.
		slow := transport.FuncStreamDialer(func(context.Context, string) (transport.StreamConn, error) {
			time.Sleep(2 * siteDialHeadStart)
			return late, nil
		})
		outConn := &fakeStreamConn{}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o := newSiteOutbound(t, connDialer(outConn), slow, dest)
		if conn, err := o.DialContext(context.Background(), "tcp", dest); err != nil || conn != net.Conn(outConn) {
			t.Fatalf("DialContext = %v, %v; want the outbound's connection", conn, err)
		}
		time.Sleep(2 * siteDialHeadStart)
		synctest.Wait()
		if !late.closed.Load() {
			t.Fatal("the site's connection that lost the race was left open")
		}
	})
}

func TestFailedSiteDialFallsBackAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		outConn := &fakeStreamConn{}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o := newSiteOutbound(t, connDialer(outConn), &recordingDialer{}, dest)
		start := time.Now()
		if conn, err := o.DialContext(context.Background(), "tcp", dest); err != nil || conn != net.Conn(outConn) {
			t.Fatalf("DialContext = %v, %v; want the outbound's connection", conn, err)
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("fallback after the site's dialer failed took %v, want no wait", took)
		}
	})
}

// The finder's dialers are func values, which can't be compared.
func TestSiteDialerFuncDialer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dials int
		fd := transport.FuncStreamDialer(func(context.Context, string) (transport.StreamConn, error) {
			dials++
			return nil, errDialRefused
		})
		f := &fakeSiteSearch{dialer: fd}
		o := &Outbound{
			createDialer: func() (transport.StreamDialer, error) { return &recordingDialer{}, nil },
			sites:        newSiteDialers(t.Context(), maxSites, nil, f.search),
		}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o.DialContext(context.Background(), "tcp", dest)
		synctest.Wait()
		for range maxSiteDialFailures {
			if _, err := o.DialContext(context.Background(), "tcp", dest); !errors.Is(err, errDialRefused) {
				t.Fatalf("DialContext: got %v, want %v", err, errDialRefused)
			}
		}
		if dials != maxSiteDialFailures {
			t.Fatalf("site dialer dialed %d times, want %d", dials, maxSiteDialFailures)
		}
	})
}

func TestCancelledSiteDialKeepsDialer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := transport.FuncStreamDialer(func(ctx context.Context, _ string) (transport.StreamConn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		f := &fakeSiteSearch{dialer: blocked}
		outboundDialer := &recordingDialer{}
		o := &Outbound{
			createDialer: func() (transport.StreamDialer, error) { return outboundDialer, nil },
			sites:        newSiteDialers(t.Context(), maxSites, nil, f.search),
		}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o.DialContext(context.Background(), "tcp", dest)
		synctest.Wait()
		for range maxSiteDialFailures + 1 {
			ctx, cancel := context.WithCancel(context.Background())
			go cancel()
			if _, err := o.DialContext(ctx, "tcp", dest); !errors.Is(err, context.Canceled) {
				t.Fatalf("DialContext: got %v, want %v", err, context.Canceled)
			}
		}
		if got := o.sites.dialerFor("www.youtube.com", 443); got == nil {
			t.Fatal("cancelled dials dropped the site's dialer")
		}
		if len(outboundDialer.addrs) != 1 {
			t.Fatalf("outbound's dialer dialed %v, want only the first dial", outboundDialer.addrs)
		}
	})
}

func TestOtherPortsDontEvictSites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &recordingDialer{}
		s := newSiteDialers(t.Context(), 1, nil, (&fakeSiteSearch{dialer: d}).search)
		s.dialerFor("youtube.com", 443)
		synctest.Wait()
		s.dialerFor("imap.example.com", 993)
		if got := s.dialerFor("youtube.com", 443); got == nil {
			t.Fatal("a dial to another port evicted a site with a dialer")
		}
	})
}

func TestEvictedSiteNotSearchedTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &recordingDialer{}
		release := make(chan struct{})
		var mu sync.Mutex
		var searched []string
		search := func(_ context.Context, site, _ string) (transport.StreamDialer, error) {
			mu.Lock()
			searched = append(searched, site)
			mu.Unlock()
			<-release
			if site != "youtube.com" {
				return nil, errDialRefused
			}
			return d, nil
		}
		s := newSiteDialers(t.Context(), 1, nil, search)
		s.dialerFor("youtube.com", 443)
		s.dialerFor("instagram.com", 443) // evicts youtube.com mid-search
		s.dialerFor("youtube.com", 443)
		synctest.Wait()
		if len(searched) != 2 {
			t.Fatalf("searched %v, want youtube.com once and instagram.com once", searched)
		}
		close(release)
		synctest.Wait()
		if got := s.dialerFor("youtube.com", 443); got == nil || got.StreamDialer != d {
			t.Fatalf("dialerFor after the search = %v, want its result despite the eviction", got)
		}
	})
}

func TestCancelledRaceReturnsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		late := &fakeStreamConn{}
		// Ignores its context, as a dialer stuck in a syscall would.
		stuck := transport.FuncStreamDialer(func(context.Context, string) (transport.StreamConn, error) {
			time.Sleep(4 * siteDialHeadStart)
			return late, nil
		})
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o := newSiteOutbound(t, &recordingDialer{}, stuck, dest)
		for range maxSiteDialFailures {
			late.closed.Store(false)
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(2*siteDialHeadStart, cancel)
			start := time.Now()
			if _, err := o.DialContext(ctx, "tcp", dest); !errors.Is(err, context.Canceled) {
				t.Fatalf("DialContext: got %v, want %v", err, context.Canceled)
			}
			if took := time.Since(start); took != 2*siteDialHeadStart {
				t.Fatalf("cancelled dial returned after %v, want %v", took, 2*siteDialHeadStart)
			}
			time.Sleep(4 * siteDialHeadStart)
			synctest.Wait()
			if !late.closed.Load() {
				t.Fatal("the connection the stuck leg returned after the cancel was left open")
			}
		}
		if o.sites.dialerFor("www.youtube.com", 443) == nil {
			t.Fatal("cancelled dials counted against the site")
		}
	})
}

// The SDK's DNS-over-TLS resolver bounds its reads only by the context's
// deadline, so both legs of a race must keep the caller's.
func TestSiteDialRaceKeepsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		seen := map[string]time.Time{}
		recordDeadline := func(name string) transport.StreamDialer {
			return transport.FuncStreamDialer(func(ctx context.Context, _ string) (transport.StreamConn, error) {
				d, _ := ctx.Deadline()
				mu.Lock()
				seen[name] = d
				mu.Unlock()
				return nil, errDialRefused
			})
		}
		dest := metadata.ParseSocksaddr("www.youtube.com:443")
		o := newSiteOutbound(t, recordDeadline("outbound"), recordDeadline("site"), dest)
		deadline := time.Now().Add(time.Minute)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		o.DialContext(ctx, "tcp", dest)
		mu.Lock()
		defer mu.Unlock()
		for _, leg := range []string{"site", "outbound"} {
			if !seen[leg].Equal(deadline) {
				t.Errorf("%s leg's deadline = %v, want the caller's, %v", leg, seen[leg], deadline)
			}
		}
	})
}

func TestPerSiteDomains(t *testing.T) {
	allowed, rejected, ok := perSiteDomains(nil)
	if !ok || allowed != nil || rejected != nil {
		t.Fatalf("perSiteDomains(nil) = %v, %v, %v; want every site", allowed, rejected, ok)
	}
	allowed, rejected, ok = perSiteDomains([]string{"nas.lan", "", "github.io"})
	if ok || len(rejected) != 3 {
		t.Fatalf("perSiteDomains with no site = %v, %v, %v; want per-site off and all rejected", allowed, rejected, ok)
	}
}

func TestSiteName(t *testing.T) {
	for entry, want := range map[string]string{
		"youtube.com":                   "youtube.com",
		"www.YouTube.com.":              "youtube.com",
		" youtube.com ":                 "youtube.com",
		".youtube.com":                  "youtube.com",
		"*.youtube.com":                 "youtube.com",
		"https://www.youtube.com/watch": "youtube.com",
		"youtube.com:443":               "youtube.com",
		"youtube.com/":                  "youtube.com",
		"例え.jp":                         "xn--r8jz45g.jp",
		"пример.рф":                     "xn--e1afmkfd.xn--p1ai",
		"user.github.io":                "user.github.io",
		"github.io":                     "",
		"nas.lan":                       "",
		"192.0.2.1":                     "",
		"":                              "",
	} {
		if got := siteName(entry); got != want {
			t.Errorf("siteName(%q) = %q, want %q", entry, got, want)
		}
	}
}

func TestSiteDialersSearchOnlyListedSites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSiteSearch{release: make(chan struct{}), dialer: &recordingDialer{}}
		s := newSiteDialers(t.Context(), maxSites, map[string]bool{"youtube.com": true}, f.search)
		if d := s.dialerFor("www.instagram.com", 443); d != nil {
			t.Fatalf("dialerFor(unlisted) = %v, want nil", d)
		}
		s.dialerFor("m.youtube.com", 443)
		synctest.Wait()
		if got := f.searched(); len(got) != 1 || got[0] != "m.youtube.com" {
			t.Fatalf("searched %v, want only the listed site's host", got)
		}
		if s.sites.Contains("instagram.com") {
			t.Fatal("an unlisted site took an LRU entry")
		}
		close(f.release)
		synctest.Wait()
		if d := s.dialerFor("www.youtube.com", 80); d == nil {
			t.Fatal("a listed site's dialer isn't used on another port")
		}
		if d := s.dialerFor("www.instagram.com", 80); d != nil {
			t.Fatalf("dialerFor(unlisted, 80) = %v, want nil", d)
		}
	})
}

func TestNewOutboundPerSiteDomains(t *testing.T) {
	doh := option.DNSEntryConfig{HTTPS: &option.HTTPSEntryConfig{Name: "dns.google"}}
	for _, tc := range []struct {
		name    string
		domains []string
		on      bool
		allowed map[string]bool
	}{
		{"unset", nil, true, nil},
		{"listed", []string{"www.youtube.com"}, true, map[string]bool{"youtube.com": true}},
		{"none valid", []string{"nas.lan"}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := option.OutboundOutlineOptions{Domains: []string{"example.com"}, DNSResolvers: []option.DNSEntryConfig{doh}, PerSiteDomains: tc.domains}
			out, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "outline", options)
			if err != nil {
				t.Fatalf("NewOutbound: %v", err)
			}
			o := out.(*Outbound)
			defer o.Close()
			if got := o.sites != nil; got != tc.on {
				t.Fatalf("per-site searches on = %v, want %v", got, tc.on)
			}
			if o.sites != nil && fmt.Sprint(o.sites.allowed) != fmt.Sprint(tc.allowed) {
				t.Fatalf("allowed = %v, want %v", o.sites.allowed, tc.allowed)
			}
		})
	}
}
