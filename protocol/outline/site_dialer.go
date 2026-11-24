package outline

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.getoutline.org/sdk/transport"
	"golang.org/x/net/publicsuffix"
)

const (
	// maxSites bounds how many sites keep a dialer of their own. The least
	// recently dialed site past it goes back to the outbound's dialer until
	// it's dialed again.
	maxSites = 256
	// siteSearchTimeout bounds one site's strategy search. The finder bounds
	// only each probe, so a wedged probe set would otherwise pin the search.
	siteSearchTimeout = 60 * time.Second
	// siteRetryAfter is how long a site whose search failed keeps using the
	// outbound's dialer before it's searched again.
	siteRetryAfter = 5 * time.Minute
)

// siteDialers finds a strategy for each site a user reaches. The outbound's
// own search tests its configured domains and applies the winner to every
// destination, so a strategy that reaches those domains can still be blocked
// for the sites people actually use. A site's search tests the site itself.
//
// A search runs in the background: until it succeeds, the site's dials use
// the outbound's dialer, so a site is never dialed later than it would be
// without its own search.
type siteDialers struct {
	// search runs a strategy search for site, testing host.
	search func(ctx context.Context, site, host string) (transport.StreamDialer, error)
	// ctx is the outbound's, so closing the box ends the searches.
	ctx context.Context
	now func() time.Time

	mu    sync.Mutex
	sites *lru.Cache[string, *siteEntry]
}

// siteEntry is one site's search state, guarded by siteDialers.mu.
type siteEntry struct {
	dialer      transport.StreamDialer
	searching   bool
	failedUntil time.Time
}

func newSiteDialers(ctx context.Context, size int, search func(ctx context.Context, site, host string) (transport.StreamDialer, error)) *siteDialers {
	sites, err := lru.New[string, *siteEntry](size)
	if err != nil {
		// Only a non-positive size fails, and callers pass a constant.
		panic(err)
	}
	return &siteDialers{search: search, ctx: ctx, now: time.Now, sites: sites}
}

// dialerFor returns host's site's dialer, or nil when it has none yet. A nil
// return starts a search unless one is running or one failed less than
// siteRetryAfter ago.
func (s *siteDialers) dialerFor(host string) transport.StreamDialer {
	site := siteOf(host)
	if site == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sites.Get(site)
	if !ok {
		e = &siteEntry{}
		s.sites.Add(site, e)
	}
	if e.dialer != nil || e.searching || s.now().Before(e.failedUntil) {
		return e.dialer
	}
	e.searching = true
	go s.run(e, site, host)
	return nil
}

func (s *siteDialers) run(e *siteEntry, site, host string) {
	ctx, cancel := context.WithTimeout(s.ctx, siteSearchTimeout)
	defer cancel()
	d, err := s.search(ctx, site, host)
	s.mu.Lock()
	defer s.mu.Unlock()
	e.searching = false
	if err == nil && d != nil {
		e.dialer = d
		return
	}
	e.failedUntil = s.now().Add(siteRetryAfter)
}

// siteOf returns host's registrable domain, so www.youtube.com and
// m.youtube.com share youtube.com's search, or "" for a name with none, such
// as an IP address or a single label.
func siteOf(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return site
}
