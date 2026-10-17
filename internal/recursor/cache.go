package recursor

import (
	"container/list"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Cache TTL bounds for the resolver's own data (delegations, server
// addresses, keys). Client answers are cached by the pipeline's response
// cache, not here.
const (
	maxCacheTTL = 24 * time.Hour
	minCacheTTL = 5 * time.Second
	// failTTL is how long a failed key, DS or address lookup is
	// remembered (RFC 9520 asks for at least 1 s and at most 5 min).
	failTTL = 30 * time.Second
)

// zoneCut is a delegation: a zone and its name servers as the parent
// published them, with glue limited to the parent's bailiwick.
type zoneCut struct {
	name  string
	ns    []string
	glue  map[string][]netip.AddrPort
	hints bool // the built-in or configured root hints, not yet primed
}

// keyState is a zone's validated DNSKEY set or the reason it has none.
type keyState struct {
	sec  Security
	keys []*dns.DNSKEY
	ede  uint16
	why  string
}

// referralDS is the DS evidence a referral carried (DS RRset or NSEC/NSEC3
// with signatures), kept unvalidated until the child's keys are needed.
type referralDS struct {
	zone    string // the delegating (parent) zone
	records []dns.RR
}

// addrSet is a cached set of name-server addresses; empty is a negative
// entry.
type addrSet []netip.Addr

// lru is a size-bounded cache with per-entry expiry.
type lru struct {
	mu  sync.Mutex
	max int
	m   map[string]*list.Element
	l   *list.List
}

type lruItem struct {
	key string
	val any
	exp time.Time
}

func newLRU(size int) *lru {
	return &lru{max: size, m: make(map[string]*list.Element), l: list.New()}
}

func (c *lru) get(key string, now time.Time) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	it := e.Value.(*lruItem)
	if !now.Before(it.exp) {
		c.l.Remove(e)
		delete(c.m, key)
		return nil, false
	}
	c.l.MoveToFront(e)
	return it.val, true
}

func (c *lru) set(key string, val any, ttl time.Duration, now time.Time) {
	ttl = min(max(ttl, minCacheTTL), maxCacheTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		it := e.Value.(*lruItem)
		it.val, it.exp = val, now.Add(ttl)
		c.l.MoveToFront(e)
		return
	}
	c.m[key] = c.l.PushFront(&lruItem{key: key, val: val, exp: now.Add(ttl)})
	for c.l.Len() > c.max {
		last := c.l.Back()
		c.l.Remove(last)
		delete(c.m, last.Value.(*lruItem).key)
	}
}

func (c *lru) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.l.Len()
}

func (c *lru) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]*list.Element)
	c.l.Init()
}

// Cache key prefixes.
const (
	kCut  = "c|"
	kAddr = "a|"
	kKeys = "k|"
	kDS   = "d|"
	kRefD = "r|"
)

// ttlOf returns the smallest TTL in rrs as a duration (0 for none).
func ttlOf(rrs ...dns.RR) time.Duration {
	var m uint32
	first := true
	for _, rr := range rrs {
		if rr == nil {
			continue
		}
		if t := rr.Header().Ttl; first || t < m {
			m, first = t, false
		}
	}
	return time.Duration(m) * time.Second
}
