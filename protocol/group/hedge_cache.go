package group

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"golang.org/x/net/publicsuffix"
)

// hedgePrefixBadSites is how many distinct sites in one address block must
// have failed on First before an unseen site in that block skips First too.
// Blocking by address takes out every name a CDN serves from that block, and
// this learns it without paying the commit timeout once per name.
const hedgePrefixBadSites = 2

// hedgeMaxEntries bounds the cache; past it, expired entries are dropped,
// then arbitrary ones.
const hedgeMaxEntries = 4096

type hedgeVerdict uint8

const (
	hedgeUnknown hedgeVerdict = iota
	hedgeGood
	hedgeBad
)

func (v hedgeVerdict) String() string {
	switch v {
	case hedgeGood:
		return "good"
	case hedgeBad:
		return "bad"
	}
	return "unknown"
}

// hedgeKey is what a hedge remembers First's results by.
type hedgeKey struct {
	// site is the registered domain (eTLD+1), so every host a CDN or site
	// uses under one name, such as rr1---sn-x.googlevideo.com, shares one
	// entry. Without a name it is the address block. Empty disables caching.
	site    string
	network string
	// prefix is the destination's /24 (IPv4) or /48 (IPv6), when known.
	prefix netip.Prefix
	// gen is the cache generation the key was made in. A result from an
	// earlier generation, a connection started on another network, is
	// dropped.
	gen uint64
}

// hedgeKeyFor builds the key for a dial. The name comes from the destination,
// else from the routed connection's sniffed domain when that metadata is for
// this destination: a nested dial can carry a parent connection's unrelated
// domain.
func hedgeKeyFor(ctx context.Context, network string, destination M.Socksaddr) hedgeKey {
	k := hedgeKey{network: network}
	addr := destination.Unwrap().Addr
	if addr.IsValid() {
		bits := 24
		if addr.Is6() {
			bits = 48
		}
		k.prefix, _ = addr.Prefix(bits)
	}
	domain := destination.Fqdn
	if domain == "" && addr.IsValid() {
		if md := adapter.ContextFrom(ctx); md != nil && md.Destination.Unwrap().Addr == addr {
			domain = md.Domain
		}
	}
	switch {
	case domain != "":
		k.site = registeredDomain(domain)
	case k.prefix.IsValid():
		k.site = "ip/" + k.prefix.String()
	}
	return k
}

// registeredDomain returns the eTLD+1 of domain, or domain itself when it has
// none (a bare TLD, an IP literal, a single-label name).
func registeredDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if etld1, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return etld1
	}
	return domain
}

type hedgeSiteKey struct{ site, network string }

type hedgePrefixKey struct {
	prefix  netip.Prefix
	network string
}

type hedgeEntry struct {
	verdict hedgeVerdict
	expires time.Time
}

// hedgeCache remembers which sites First worked for. One cache is shared by
// every hedge in a box, since whether a site needs a proxy depends on the site
// and the network, not on which hedge asked.
type hedgeCache struct {
	now func() time.Time

	mu    sync.Mutex
	gen   uint64
	sites map[hedgeSiteKey]hedgeEntry
	// badSites holds, per address block, the sites in it that failed on
	// First and when each failure expires.
	badSites map[hedgePrefixKey]map[string]time.Time
}

func newHedgeCache() *hedgeCache {
	return &hedgeCache{
		now:      time.Now,
		sites:    map[hedgeSiteKey]hedgeEntry{},
		badSites: map[hedgePrefixKey]map[string]time.Time{},
	}
}

// sharedHedgeCache returns the box's cache, creating it on first use. Without
// a service registry in ctx the hedge gets a private one.
func sharedHedgeCache(ctx context.Context) *hedgeCache {
	if service.RegistryFromContext(ctx) == nil {
		return newHedgeCache()
	}
	if c := service.FromContext[*hedgeCache](ctx); c != nil {
		return c
	}
	c := newHedgeCache()
	service.MustRegister[*hedgeCache](ctx, c)
	return c
}

// verdict reports what is known about First for k: the site's own entry if
// it has one, else bad when enough other sites in its address block failed.
func (c *hedgeCache) verdict(k hedgeKey) hedgeVerdict {
	if k.site == "" {
		return hedgeUnknown
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	sk := hedgeSiteKey{k.site, k.network}
	if e, ok := c.sites[sk]; ok {
		if now.Before(e.expires) {
			return e.verdict
		}
		delete(c.sites, sk)
	}
	if !k.prefix.IsValid() {
		return hedgeUnknown
	}
	pk := hedgePrefixKey{k.prefix, k.network}
	bad := 0
	for site, expires := range c.badSites[pk] {
		if !now.Before(expires) {
			delete(c.badSites[pk], site)
			continue
		}
		if site != k.site {
			bad++
		}
	}
	if len(c.badSites[pk]) == 0 {
		delete(c.badSites, pk)
	}
	if bad >= hedgePrefixBadSites {
		return hedgeBad
	}
	return hedgeUnknown
}

// generation returns the current generation, for stamping keys.
func (c *hedgeCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *hedgeCache) mark(k hedgeKey, v hedgeVerdict, ttl time.Duration) {
	if k.site == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if k.gen != c.gen {
		return // learned on a network the cache has since forgotten
	}
	expires := c.now().Add(ttl)
	c.sites[hedgeSiteKey{k.site, k.network}] = hedgeEntry{verdict: v, expires: expires}
	if k.prefix.IsValid() {
		pk := hedgePrefixKey{k.prefix, k.network}
		switch v {
		case hedgeBad:
			if c.badSites[pk] == nil {
				c.badSites[pk] = map[string]time.Time{}
			}
			c.badSites[pk][k.site] = expires
		case hedgeGood:
			delete(c.badSites[pk], k.site)
			if len(c.badSites[pk]) == 0 {
				delete(c.badSites, pk)
			}
		}
	}
	c.trimLocked()
}

// trimLocked bounds both indexes. They grow independently: one site failing
// across many address blocks adds a block group each time but only one site
// entry. Caller holds mu.
func (c *hedgeCache) trimLocked() {
	now := c.now()
	if len(c.sites) > hedgeMaxEntries {
		for k, e := range c.sites {
			if !now.Before(e.expires) {
				delete(c.sites, k)
			}
		}
		for k := range c.sites {
			if len(c.sites) <= hedgeMaxEntries {
				break
			}
			delete(c.sites, k)
		}
	}
	records := 0
	for _, sites := range c.badSites {
		records += len(sites)
	}
	if records <= hedgeMaxEntries {
		return
	}
	for pk, sites := range c.badSites {
		for site, expires := range sites {
			if !now.Before(expires) {
				delete(sites, site)
				records--
			}
		}
		if len(sites) == 0 {
			delete(c.badSites, pk)
		}
	}
	for pk, sites := range c.badSites {
		if records <= hedgeMaxEntries {
			break
		}
		records -= len(sites)
		delete(c.badSites, pk)
	}
}

// clear forgets everything, for a network change: what is blocked differs
// between networks. It starts a new generation, so results from connections
// made before it are dropped.
func (c *hedgeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.sites = map[hedgeSiteKey]hedgeEntry{}
	c.badSites = map[hedgePrefixKey]map[string]time.Time{}
}
