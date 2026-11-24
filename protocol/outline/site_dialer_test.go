package outline

import (
	"context"
	"errors"
	"sync"
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
		"192.0.2.1":        "",
		"2001:db8::1":      "",
		"localhost":        "",
		"":                 "",
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
		s := newSiteDialers(t.Context(), maxSites, f.search)

		// Dials during the search get no site dialer, and share one search,
		// including a sibling subdomain.
		for _, host := range []string{"www.youtube.com", "www.youtube.com", "m.youtube.com"} {
			if got := s.dialerFor(host); got != nil {
				t.Fatalf("dialerFor(%q) during the search = %v, want nil", host, got)
			}
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 1 || got[0] != "www.youtube.com" {
			t.Fatalf("searched %v, want one search of www.youtube.com", got)
		}

		close(f.release)
		synctest.Wait()
		if got := s.dialerFor("m.youtube.com"); got != d {
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
		s := newSiteDialers(t.Context(), maxSites, f.search)

		s.dialerFor("www.youtube.com")
		synctest.Wait()
		time.Sleep(siteRetryAfter - time.Second)
		if got := s.dialerFor("www.youtube.com"); got != nil {
			t.Fatalf("dialerFor after a failed search = %v, want nil", got)
		}
		synctest.Wait()
		if got := f.searched(); len(got) != 1 {
			t.Fatalf("searched %v before siteRetryAfter, want 1 search", got)
		}

		time.Sleep(time.Second)
		s.dialerFor("www.youtube.com")
		synctest.Wait()
		if got := f.searched(); len(got) != 2 {
			t.Fatalf("searched %v after siteRetryAfter, want 2 searches", got)
		}
	})
}

func TestSiteDialersSkipHostsWithoutSite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSiteSearch{dialer: &recordingDialer{}}
		s := newSiteDialers(t.Context(), maxSites, f.search)
		for _, host := range []string{"192.0.2.1", "localhost"} {
			if got := s.dialerFor(host); got != nil {
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
		s := newSiteDialers(t.Context(), 1, f.search)
		s.dialerFor("youtube.com")
		synctest.Wait()
		s.dialerFor("instagram.com")
		synctest.Wait()
		// youtube.com was evicted, so it's searched again.
		if got := s.dialerFor("youtube.com"); got != nil {
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
		s := newSiteDialers(ctx, maxSites, f.search)
		s.dialerFor("youtube.com")
		synctest.Wait()
		cancel()
		synctest.Wait()
		// The cancelled search counts as failed, so the site keeps the
		// outbound's dialer.
		if got := s.dialerFor("youtube.com"); got != nil {
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
			sites:        newSiteDialers(t.Context(), maxSites, f.search),
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

		dial("192.0.2.1:443")
		if !outboundDialer.dialed("192.0.2.1:443") {
			t.Fatalf("IP dial went to %v, want the outbound's dialer", outboundDialer.addrs)
		}
	})
}

func TestNewOutboundPerSiteOption(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name    string
		perSite *bool
		want    bool
	}{
		{"unset", nil, true},
		{"true", &yes, true},
		{"false", &no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := option.OutboundOutlineOptions{Domains: []string{"example.com"}, PerSite: tc.perSite}
			out, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "outline", options)
			if err != nil {
				t.Fatalf("NewOutbound: %v", err)
			}
			if got := out.(*Outbound).sites != nil; got != tc.want {
				t.Fatalf("per-site searches on = %v, want %v", got, tc.want)
			}
		})
	}
}
