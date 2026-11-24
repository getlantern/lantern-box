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
	// outbound's dialer before it's searched again. It doubles with each
	// failure in a row, up to siteRetryMax, so a site blocked outright isn't
	// probed every few minutes.
	siteRetryAfter = 5 * time.Minute
	siteRetryMax   = time.Hour
	// maxSiteSearches bounds the searches running at once. A page load can
	// reach a dozen sites, and each search races every resolver and probes
	// up to every TLS strategy. A site dialed while all slots are busy is
	// searched on a later dial.
	maxSiteSearches = 4
	// maxSiteDialFailures is how many dials in a row a site's dialer may fail
	// before the site goes back to the outbound's dialer and is searched
	// again, as when a resolver it chose is blocked later.
	maxSiteDialFailures = 3
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
	// ctx ends the searches when the outbound closes.
	ctx context.Context
	now func() time.Time

	mu        sync.Mutex
	sites     *lru.Cache[string, *siteEntry]
	searching int
	limit     int
}

// siteEntry is one site's state, guarded by siteDialers.mu.
type siteEntry struct {
	dialer       transport.StreamDialer
	dialFailures int
	searching    bool
	// searchFailures counts failed searches in a row, and failedUntil is
	// when the site may be searched again.
	searchFailures int
	failedUntil    time.Time
}

func newSiteDialers(ctx context.Context, size int, search func(ctx context.Context, site, host string) (transport.StreamDialer, error)) *siteDialers {
	sites, err := lru.New[string, *siteEntry](size)
	if err != nil {
		// Only a non-positive size fails, and callers pass a constant.
		panic(err)
	}
	return &siteDialers{search: search, ctx: ctx, now: time.Now, sites: sites, limit: maxSiteSearches}
}

// dialerFor returns host's site's dialer, or nil when it has none yet. A nil
// return for a dial to port 443 starts a search of host, unless one is
// running, the site's last search failed less than its retry delay ago, or
// maxSiteSearches are already running. Only 443 starts one: a search tests
// HTTPS on 443, which a host dialed on another port, such as an IMAP server,
// needn't serve.
func (s *siteDialers) dialerFor(host string, port uint16) transport.StreamDialer {
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
	if e.dialer != nil || e.searching || port != 443 || s.now().Before(e.failedUntil) || s.searching >= s.limit {
		return e.dialer
	}
	e.searching = true
	s.searching++
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
	s.searching--
	if err == nil && d != nil {
		e.dialer, e.dialFailures, e.searchFailures = d, 0, 0
		return
	}
	e.searchFailures++
	e.failedUntil = s.now().Add(retryDelay(e.searchFailures))
}

// dialed records how a dial through d, host's site's dialer, went. After
// maxSiteDialFailures failures in a row the site drops d, goes back to the
// outbound's dialer and is searched again after siteRetryAfter.
func (s *siteDialers) dialed(host string, d transport.StreamDialer, err error) {
	site := siteOf(host)
	if site == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sites.Peek(site)
	// A search since the dial may have replaced the dialer.
	if !ok || e.dialer != d {
		return
	}
	if err == nil {
		e.dialFailures = 0
		return
	}
	e.dialFailures++
	if e.dialFailures >= maxSiteDialFailures {
		e.dialer, e.dialFailures = nil, 0
		e.failedUntil = s.now().Add(siteRetryAfter)
	}
}

func retryDelay(failures int) time.Duration {
	d := siteRetryAfter
	for i := 1; i < failures && d < siteRetryMax; i++ {
		d *= 2
	}
	return min(d, siteRetryMax)
}

// specialUseTLDs are top-level names reserved for local or non-DNS use
// (RFC 6761, RFC 6762, RFC 9476, home.arpa under RFC 8375), which ICANN's
// list doesn't mark as private.
var specialUseTLDs = map[string]bool{
	"alt": true, "arpa": true, "example": true, "internal": true, "invalid": true,
	"local": true, "localhost": true, "onion": true, "test": true,
}

// siteOf returns host's registrable domain, so www.youtube.com and
// m.youtube.com share youtube.com's search, or "" for a name with none: an
// IP address, a single label, or a local name such as nas.lan or
// printer.home.arpa, which a search would send to public resolvers.
func siteOf(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	if _, icann := publicsuffix.PublicSuffix(host); !icann {
		return ""
	}
	if specialUseTLDs[host[strings.LastIndexByte(host, '.')+1:]] {
		return ""
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return site
}
