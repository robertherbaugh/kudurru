package cache

import (
	"context"
	"hash/maphash"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"golang.org/x/sync/singleflight"
)

// Kind separates positive answers from negative ones (NXDOMAIN, NODATA).
type Kind uint8

// Kinds.
const (
	Positive Kind = iota
	Negative
)

func (k Kind) String() string {
	if k == Negative {
		return "negative"
	}
	return "positive"
}

// Eviction reasons (the reason label of kudurru_cache_evictions_total).
const (
	ReasonCapacity = "capacity" // LRU victim when a shard is full
	ReasonExpired  = "expired"  // past expiry plus the stale window
	ReasonFlush    = "flush"    // Flush or Delete
)

// Options configure a Cache. Zero values disable the matching feature.
type Options struct {
	// MaxEntries bounds the cache; entries beyond it are evicted least
	// recently used first. It must be positive. The bound is enforced per
	// shard (MaxEntries/shards each, up to 256 shards of at least 64
	// entries), so it is never exceeded but a full cache holds slightly
	// fewer entries (about 0.5 % fewer at 200 k) and LRU order is per shard.
	MaxEntries int
	// StaleTTL is how long past expiry an entry is kept and may be served
	// stale (RFC 8767); 0 drops entries when they expire.
	StaleTTL time.Duration
	// PrefetchThreshold is the number of hits within PrefetchWindow that
	// makes an entry hot; 0 disables prefetch.
	PrefetchThreshold int
	PrefetchWindow    time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Metrics, when non-nil, are kept up to date.
	Metrics *Metrics
}

// State of a looked-up entry.
type State uint8

// States.
const (
	Fresh State = iota + 1
	Stale
)

// Lookup is a hit returned by Get.
type Lookup struct {
	Metadata Metadata
	// Wire is the stored message; it is shared and must not be modified.
	Wire []byte
	Kind Kind
	// Age is the time since the entry was stored, TTL its lifetime then.
	Age, TTL time.Duration
	State    State
	// Prefetch is set on a fresh hit that made a hot entry due for refresh;
	// the caller owns the refresh and must end it with Set or
	// ReleasePrefetch.
	Prefetch bool
	// Hold is set on a stale hit while a recent refresh failure is being
	// honoured (MarkRefreshFailed): serve stale without asking upstream.
	Hold bool
}

// Cache is a sharded LRU message cache with TTLs, a stale window, prefetch
// accounting and in-flight coalescing. It stores packed messages as bytes
// and knows nothing about DNS beyond the key.
type Cache struct {
	publication sync.RWMutex
	generation  atomic.Uint64
	shards      []shard
	mask        uint64
	seed        maphash.Seed
	staleTTL    atomic.Int64
	threshold   atomic.Uint32
	window      atomic.Int64
	now         func() time.Time
	m           *Metrics
	flight      singleflight.Group
}

type entry struct {
	metadata    Metadata
	restored    bool
	key         Key
	wire        []byte
	stored      int64 // unix nanoseconds
	expires     int64
	staleUntil  int64
	holdUntil   int64
	windowStart int64
	hits        uint32
	kind        Kind
	prefetching bool
	prev, next  *entry
}

// Metadata accompanies raw wire data through hits and restart.
type Metadata struct {
	Scope    pipeline.CacheScope
	Security pipeline.Security
	Upstream string
}

// Now returns this cache's configured clock.
func (c *Cache) Now() time.Time { return c.now() }

// Generation identifies the current cache invalidation epoch.
func (c *Cache) Generation() uint64 { return c.generation.Load() }

type shard struct {
	mu         sync.Mutex
	m          map[Key]*entry
	head, tail *entry // most recently used at head
	capacity   int
	_          [32]byte // keep hot shard locks on separate cache lines
}

// New returns an empty cache.
func New(opts Options) *Cache {
	maxEntries := max(opts.MaxEntries, 1)
	// Enough shards to spread lock contention, few enough that each keeps a
	// meaningful LRU (at least 64 entries per shard).
	n := 1
	for n < 256 && maxEntries/(n*2) >= 64 {
		n *= 2
	}
	c := &Cache{
		shards: make([]shard, n),
		mask:   uint64(n - 1), //nolint:gosec // n is a small power of two
		seed:   maphash.MakeSeed(),
		now:    opts.Now,
		m:      opts.Metrics,
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.setLimits(opts)
	per := (maxEntries + n - 1) / n
	for i := range c.shards {
		c.shards[i].m = make(map[Key]*entry)
		c.shards[i].capacity = per
	}
	return c
}

func (c *Cache) setLimits(opts Options) {
	c.staleTTL.Store(int64(max(opts.StaleTTL, 0)))
	c.threshold.Store(uint32(max(opts.PrefetchThreshold, 0))) //nolint:gosec // non-negative int
	c.window.Store(int64(opts.PrefetchWindow))
}

// Reconfigure applies new limits without dropping the cache (settings hot
// reload): the stale window and prefetch rules apply to entries stored
// from now on, and MaxEntries resizes every shard, evicting the least
// recently used entries beyond the new bound at once. The shard count
// chosen by New stays, so a much larger bound means larger shards.
// MaxEntries <= 0 keeps the current size (the cache stage bypasses a
// disabled cache instead). Now and Metrics are ignored.
func (c *Cache) Reconfigure(opts Options) {
	c.setLimits(opts)
	c.Resize(opts.MaxEntries)
}

// Resize changes only storage capacity. Captured serving uses it without
// replacing the stale/prefetch policy of independently held requests.
// A nonpositive bound preserves capacity; the captured stage controls bypass.
func (c *Cache) Resize(maxEntries int) {
	if maxEntries <= 0 {
		return
	}
	n := len(c.shards)
	per := max((maxEntries+n-1)/n, 1)
	for i := range c.shards {
		sh := &c.shards[i]
		var evicted [2]int
		sh.mu.Lock()
		sh.capacity = per
		for len(sh.m) > sh.capacity && sh.tail != nil {
			evicted[sh.tail.kind]++
			sh.remove(sh.tail)
		}
		sh.mu.Unlock()
		for kind, k := range evicted {
			c.m.evictedN(ReasonCapacity, Kind(kind), k) //nolint:gosec // 0 or 1
		}
	}
}

func (c *Cache) shard(k Key) *shard {
	return &c.shards[maphash.String(c.seed, string(k))&c.mask]
}

func (c *Cache) nanos() int64 { return c.now().UnixNano() }

// Get looks k up for a query. Entries past their stale window are removed
// and reported as a miss. Every call counts as a cache request and a fresh
// hit is counted here; a stale hit is counted by the caller with
// CountStaleHit once it knows it served the stale data (a successful
// refresh makes it a miss). A fresh hit moves the entry to the front of the
// LRU and feeds the prefetch accounting.
func (c *Cache) Get(k Key) (Lookup, bool) {
	c.m.request()
	now := c.nanos()
	sh := c.shard(k)
	sh.mu.Lock()
	e := sh.m[k]
	if e == nil {
		sh.mu.Unlock()
		return Lookup{}, false
	}
	if now >= e.staleUntil || now < e.stored || !e.metadata.Security.AvailableAt(time.Unix(0, now)) {
		kind := e.kind
		sh.remove(e)
		sh.mu.Unlock()
		c.m.evicted(ReasonExpired, kind)
		return Lookup{}, false
	}
	sh.moveFront(e)
	l := Lookup{Metadata: e.metadata, Wire: e.wire, Kind: e.kind, Age: time.Duration(now - e.stored), TTL: time.Duration(e.expires - e.stored)}
	if now < e.expires {
		l.State = Fresh
		if threshold := c.threshold.Load(); threshold > 0 {
			if now-e.windowStart > c.window.Load() {
				e.windowStart, e.hits = now, 0
			}
			e.hits++
			trigger := max(int64(time.Second), (e.expires-e.stored)/10)
			if e.hits >= threshold && e.expires-now <= trigger && !e.prefetching {
				e.prefetching = true
				l.Prefetch = true
			}
		}
		sh.mu.Unlock()
		c.m.hit(l.Kind, false)
		return l, true
	}
	l.State = Stale
	l.Hold = now < e.holdUntil
	sh.mu.Unlock()
	return l, true
}

// CountStaleHit records that a stale lookup was answered from the cache.
func (c *Cache) CountStaleHit(kind Kind) { c.m.hit(kind, true) }

// CountPrefetch records that a prefetch was started.
func (c *Cache) CountPrefetch() { c.m.Prefetched() }

// Peek returns the entry for k without any side effect: no request or hit
// accounting, no LRU movement, no prefetch claim and no expiry removal.
// Entries past their stale window are reported absent. It is meant for
// debugging APIs and tests.
func (c *Cache) Peek(k Key) (Lookup, bool) {
	now := c.nanos()
	sh := c.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := sh.m[k]
	if e == nil || now >= e.staleUntil || now < e.stored || !e.metadata.Security.AvailableAt(time.Unix(0, now)) {
		return Lookup{}, false
	}
	l := Lookup{Metadata: e.metadata, Wire: e.wire, Kind: e.kind, Age: time.Duration(now - e.stored), TTL: time.Duration(e.expires - e.stored), State: Fresh}
	if now >= e.expires {
		l.State = Stale
		l.Hold = now < e.holdUntil
	}
	return l, true
}

// Set stores wire under k for ttl and takes ownership of wire. An existing
// entry is replaced (and its prefetch claim released).
func (c *Cache) Set(k Key, wire []byte, kind Kind, ttl time.Duration) {
	c.SetWithMetadata(k, wire, kind, ttl, Metadata{})
}

// SetWithMetadata stores raw wire data and its provenance in the current epoch.
func (c *Cache) SetWithMetadata(k Key, wire []byte, kind Kind, ttl time.Duration, metadata Metadata) {
	c.SetIfGeneration(k, wire, kind, ttl, metadata, c.Generation())
}

// SetIfGeneration fences Flush/delete against detached writes.
func (c *Cache) SetIfGeneration(k Key, wire []byte, kind Kind, ttl time.Duration, metadata Metadata, generation uint64) {
	c.publication.RLock()
	defer c.publication.RUnlock()
	if generation != c.Generation() || !metadata.Security.AvailableAt(c.now()) {
		return
	}
	if ttl <= 0 {
		return
	}
	now := c.nanos()
	exp := now + int64(ttl)
	stale := exp + c.staleTTL.Load()
	if !metadata.Security.ValidUntil.IsZero() {
		deadline := metadata.Security.ValidUntil.UnixNano()
		exp = min(exp, deadline)
		stale = min(stale, deadline)
	}
	c.insert(&entry{metadata: metadata, key: k, wire: wire, kind: kind, stored: now, expires: exp, staleUntil: stale, windowStart: now})
}

// insert adds or replaces e and evicts beyond capacity.
func (c *Cache) insert(e *entry) {
	sh := c.shard(e.key)
	var evicted, expired []Kind
	replaced, oldKind := false, Positive
	sh.mu.Lock()
	if old := sh.m[e.key]; old != nil {
		// Keep the hit history so a refreshed hot entry stays hot.
		e.hits, e.windowStart = old.hits, old.windowStart
		replaced, oldKind = true, old.kind
		sh.remove(old)
	}
	sh.m[e.key] = e
	sh.pushFront(e)
	now := c.nanos()
	for len(sh.m) > sh.capacity && sh.tail != nil {
		victim := sh.tail
		if now >= victim.staleUntil {
			expired = append(expired, victim.kind)
		} else {
			evicted = append(evicted, victim.kind)
		}
		sh.remove(victim)
	}
	sh.mu.Unlock()
	if replaced {
		c.m.entries(oldKind, -1)
	}
	c.m.entries(e.kind, 1)
	for _, k := range evicted {
		c.m.evicted(ReasonCapacity, k)
	}
	for _, k := range expired {
		c.m.evicted(ReasonExpired, k)
	}
}

// ReleasePrefetch gives up a prefetch claim without storing (the refresh
// failed); the next qualifying hit may claim it again.
func (c *Cache) ReleasePrefetch(k Key) {
	sh := c.shard(k)
	sh.mu.Lock()
	if e := sh.m[k]; e != nil {
		e.prefetching = false
	}
	sh.mu.Unlock()
}

// MarkRefreshFailed makes stale lookups of k report Hold for d: every
// upstream just failed, so for a while stale data is served immediately
// instead of waiting on upstreams again (RFC 8767 §4, failure recheck).
func (c *Cache) MarkRefreshFailed(k Key, d time.Duration) {
	until := c.nanos() + int64(d)
	sh := c.shard(k)
	sh.mu.Lock()
	if e := sh.m[k]; e != nil {
		e.holdUntil = until
		e.prefetching = false
	}
	sh.mu.Unlock()
}

// Delete removes k.
func (c *Cache) Delete(k Key) {
	c.publication.Lock()
	defer c.publication.Unlock()
	c.generation.Add(1)
	sh := c.shard(k)
	sh.mu.Lock()
	e := sh.m[k]
	if e != nil {
		sh.remove(e)
	}
	sh.mu.Unlock()
	if e != nil {
		c.m.evicted(ReasonFlush, e.kind)
	}
}

// Flush removes every entry.
func (c *Cache) Flush() {
	c.FlushCount()
}

// FlushCount removes every entry and returns the number actually removed.
// It fences earlier detached fills even when the cache is already empty.
func (c *Cache) FlushCount() int {
	c.publication.Lock()
	defer c.publication.Unlock()
	c.generation.Add(1)
	total := 0
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		var counts [2]int
		for _, e := range sh.m {
			counts[e.kind]++
		}
		sh.m = make(map[Key]*entry)
		sh.head, sh.tail = nil, nil
		sh.mu.Unlock()
		for kind, n := range counts {
			c.m.evictedN(ReasonFlush, Kind(kind), n) //nolint:gosec // 0 or 1
			total += n
		}
	}
	return total
}

// FlushName removes the exact name across all scopes, types, classes, flags and
// ECS subnets, including stale entries. Case and a final dot are normalized;
// root means only root, and an empty name does nothing. Valid no-op names still
// fence earlier detached fills. Like FlushZone, this rare action scans the cache.
func (c *Cache) FlushName(name string) int {
	if name == "" {
		return 0
	}
	// Match MakeKey's ASCII-only folding; other DNS label bytes are opaque.
	normalized := []byte(name)
	for i, b := range normalized {
		if b >= 'A' && b <= 'Z' {
			normalized[i] = b + ('a' - 'A')
		}
	}
	if normalized[len(normalized)-1] != '.' {
		normalized = append(normalized, '.')
	}
	name = string(normalized)
	c.publication.Lock()
	defer c.publication.Unlock()
	c.generation.Add(1)
	total := 0
	for i := range c.shards {
		sh := &c.shards[i]
		var counts [2]int
		sh.mu.Lock()
		for key, e := range sh.m {
			if parts, ok := key.Parts(); ok && parts.Name == name {
				counts[e.kind]++
				sh.remove(e)
			}
		}
		sh.mu.Unlock()
		for kind, n := range counts {
			c.m.evictedN(ReasonFlush, Kind(kind), n) //nolint:gosec // 0 or 1
			total += n
		}
	}
	return total
}

// FlushZone removes every entry whose name is apex or below it (any type,
// class, flags or ECS subnet) and returns how many went. apex is a domain
// name, folded to lower case; "." matches every entry. It walks the whole
// cache, so it is for rare operator actions such as deleting a zone.
func (c *Cache) FlushZone(apex string) int {
	c.publication.Lock()
	defer c.publication.Unlock()
	c.generation.Add(1)
	apex = strings.ToLower(apex)
	if !strings.HasSuffix(apex, ".") {
		apex += "."
	}
	total := 0
	for i := range c.shards {
		sh := &c.shards[i]
		var counts [2]int
		sh.mu.Lock()
		for k, e := range sh.m {
			p, ok := k.Parts()
			if !ok {
				continue
			}
			if apex == "." || p.Name == apex || strings.HasSuffix(p.Name, "."+apex) {
				counts[e.kind]++
				sh.remove(e)
			}
		}
		sh.mu.Unlock()
		for kind, n := range counts {
			c.m.evictedN(ReasonFlush, Kind(kind), n) //nolint:gosec // 0 or 1
			total += n
		}
	}
	return total
}

// Len returns the number of entries, including stale ones.
func (c *Cache) Len() int {
	n := 0
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// Sweep removes entries past their stale window and returns how many.
func (c *Cache) Sweep() int {
	now := c.nanos()
	total := 0
	for i := range c.shards {
		sh := &c.shards[i]
		var counts [2]int
		sh.mu.Lock()
		for _, e := range sh.m {
			if now >= e.staleUntil {
				counts[e.kind]++
				sh.remove(e)
			}
		}
		sh.mu.Unlock()
		for kind, n := range counts {
			c.m.evictedN(ReasonExpired, Kind(kind), n) //nolint:gosec // 0 or 1
			total += n
		}
	}
	return total
}

// Run sweeps every interval (1 min when zero) until ctx ends.
func (c *Cache) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.Sweep()
		}
	}
}

// Coalesce runs fn once for concurrent callers with the same key and
// delivers the shared result on the returned channel (singleflight). fn
// should be bounded by its own deadline: callers may stop waiting early,
// but fn runs to completion.
func (c *Cache) Coalesce(k Key, fn func() (any, error)) <-chan singleflight.Result {
	return c.flight.DoChan(strconv.FormatUint(c.Generation(), 10)+":"+string(k), fn)
}

// List operations; the shard lock is held.

func (sh *shard) pushFront(e *entry) {
	e.prev, e.next = nil, sh.head
	if sh.head != nil {
		sh.head.prev = e
	}
	sh.head = e
	if sh.tail == nil {
		sh.tail = e
	}
}

func (sh *shard) unlink(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		sh.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		sh.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (sh *shard) moveFront(e *entry) {
	if sh.head == e {
		return
	}
	sh.unlink(e)
	sh.pushFront(e)
}

func (sh *shard) remove(e *entry) {
	sh.unlink(e)
	delete(sh.m, e.key)
}
