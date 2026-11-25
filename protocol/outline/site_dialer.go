package outline

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.getoutline.org/sdk/transport"
	"golang.org/x/net/idna"
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
	// again, as when a resolver it chose is blocked later. A dial fails only
	// on resolution or connect: the smart dialer returns once TCP connects,
	// so a strategy blocked later, at the TLS handshake, isn't counted.
	maxSiteDialFailures = 3
	// siteDialHeadStart is how long a site's dialer has to connect before the
	// outbound's dialer races it. The hedge gives proxyless a second in all,
	// so a site's dialer that stalls must leave most of it to the outbound's.
	siteDialHeadStart = 300 * time.Millisecond
)

// errSiteDialSlow counts against a site's dialer when the outbound's dialer
// connected first.
var errSiteDialSlow = errors.New("outline: the site's dialer lost to the outbound's")

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

	mu    sync.Mutex
	sites *lru.Cache[string, *siteEntry]
	// inFlight holds the sites being searched, apart from sites, so an
	// eviction can't start a second search of a site.
	inFlight map[string]bool
	limit    int
	// allowed, when not nil, holds the only sites that get a dialer of
	// their own. It isn't changed after construction.
	allowed map[string]bool
}

// siteDialer is a site's dialer. Dialers are compared by this pointer: the
// finder's are func values, which panic when compared.
type siteDialer struct {
	transport.StreamDialer
}

// siteEntry is one site's state, guarded by siteDialers.mu.
type siteEntry struct {
	dialer       *siteDialer
	dialFailures int
	// searchFailures counts failed searches in a row, and failedUntil is
	// when the site may be searched again.
	searchFailures int
	failedUntil    time.Time
}

func newSiteDialers(ctx context.Context, size int, allowed map[string]bool, search func(ctx context.Context, site, host string) (transport.StreamDialer, error)) *siteDialers {
	sites, err := lru.New[string, *siteEntry](size)
	if err != nil {
		// Only a non-positive size fails, and callers pass a constant.
		panic(err)
	}
	return &siteDialers{
		search: search, ctx: ctx, now: time.Now,
		sites: sites, inFlight: map[string]bool{}, limit: maxSiteSearches,
		allowed: allowed,
	}
}

// dialerFor returns host's site's dialer, or nil when it has none yet. A nil
// return for a dial to port 443 starts a search of host, unless one is
// running, the site's last search failed less than its retry delay ago, or
// maxSiteSearches are already running. Only 443 starts one: a search tests
// HTTPS on 443, which a host dialed on another port, such as an IMAP server,
// needn't serve. Dials to other ports don't add the site either, so they
// can't evict sites that have a dialer.
func (s *siteDialers) dialerFor(host string, port uint16) *siteDialer {
	site := siteOf(host)
	if site == "" || (s.allowed != nil && !s.allowed[site]) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if port != 443 {
		if e, ok := s.sites.Get(site); ok {
			return e.dialer
		}
		return nil
	}
	e, ok := s.sites.Get(site)
	if !ok {
		e = &siteEntry{}
		s.sites.Add(site, e)
	}
	if e.dialer != nil || s.inFlight[site] || s.now().Before(e.failedUntil) || len(s.inFlight) >= s.limit {
		return e.dialer
	}
	s.inFlight[site] = true
	go s.run(site, host)
	return nil
}

// run searches site and records the result on the site's entry at the time
// it finishes. A found dialer gets a new entry if the old one was evicted
// meanwhile.
func (s *siteDialers) run(site, host string) {
	ctx, cancel := context.WithTimeout(s.ctx, siteSearchTimeout)
	defer cancel()
	d, err := s.search(ctx, site, host)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, site)
	found := err == nil && d != nil
	e, ok := s.sites.Peek(site)
	if !ok {
		// The site was evicted during the search. A failure isn't worth
		// evicting another site to record.
		if !found {
			return
		}
		e = &siteEntry{}
		s.sites.Add(site, e)
	}
	if found {
		e.dialer, e.dialFailures, e.searchFailures = &siteDialer{d}, 0, 0
		return
	}
	e.searchFailures++
	e.failedUntil = s.now().Add(retryDelay(e.searchFailures))
}

// dialed records how a dial through d, host's site's dialer, went. After
// maxSiteDialFailures failures in a row the site drops d, goes back to the
// outbound's dialer and is searched again after siteRetryAfter.
func (s *siteDialers) dialed(host string, d *siteDialer, err error) {
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

// perSiteDomains returns the sites domains name and true; nil and true when
// domains is empty, meaning every site; and false when domains names no
// site, so per-site searches are off. rejected holds the entries that name no
// site.
func perSiteDomains(domains []string) (allowed map[string]bool, rejected []string, ok bool) {
	if len(domains) == 0 {
		return nil, nil, true
	}
	allowed = make(map[string]bool, len(domains))
	for _, d := range domains {
		if site := siteName(d); site != "" {
			allowed[site] = true
		} else {
			rejected = append(rejected, d)
		}
	}
	return allowed, rejected, len(allowed) > 0
}

// siteName returns the site, as siteOf, that a configured entry names, or ""
// for none. It takes the forms an entry is likely written in: with spaces
// around it, as a URL or with a port, with a "*." or "." prefix as other
// sing-box options take domains, or in Unicode, which dialed hosts carry as
// punycode.
func siteName(entry string) string {
	s := strings.TrimPrefix(strings.TrimSpace(entry), "//")
	// A scheme only before the path, or a URL in a query would name the site,
	// and only a valid one, or a list joined by commas or spaces would name
	// its last entry's site.
	if i := strings.Index(s, "://"); i >= 0 && isScheme(s[:i]) {
		if cut := strings.IndexAny(s, "/?#"); cut < 0 || i < cut {
			s = s[i+3:]
		}
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "*."), ".")
	// A name with any Unicode is mapped to the punycode dialed hosts carry,
	// under IDNA's strict rules for every label, so a hyphenated ASCII label
	// in it is rejected. An all-ASCII name skips IDNA, whose label checks
	// would reject hosts dialed in practice, such as
	// r3---sn-n4v7knll.googlevideo.com.
	if strings.IndexFunc(s, func(r rune) bool { return r >= utf8.RuneSelf }) >= 0 {
		ascii, err := idna.Lookup.ToASCII(s)
		if err != nil {
			return ""
		}
		s = ascii
	}
	s = strings.ToLower(s)
	if strings.IndexFunc(s, notHostnameRune) >= 0 {
		return ""
	}
	return siteOf(s)
}

// isScheme reports whether s is a URL scheme (RFC 3986 section 3.1): a
// letter, then letters, digits, "+", "-" or ".".
func isScheme(s string) bool {
	for i, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (i == 0 || !(r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.')) {
			return false
		}
	}
	return s != ""
}

func notHostnameRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
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
	tld := host[strings.LastIndexByte(host, '.')+1:]
	if specialUseTLDs[tld] {
		return ""
	}
	// A name is public if its suffix is ICANN's, which covers wildcard-only
	// TLDs such as mm, or if its TLD is, which covers private suffixes such
	// as appspot.com. A name under a TLD outside ICANN's list is local.
	if _, icann := publicsuffix.PublicSuffix(host); !icann {
		if _, icann := publicsuffix.PublicSuffix(tld); !icann {
			return ""
		}
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return site
}
