package ingress

import (
	"container/list"
	"errors"
	"fmt"
	"hash/maphash"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// The per-prefix QPM limiter of dev-docs/design-record.md §7.1 step 2. Every
// UDP query is charged to a token bucket per configured prefix length of
// its client address (IPv4 /32 and /24, IPv6 /128, /64 and /56 by
// default), most specific first. A bucket holds one minute's allowance
// and refills continuously at QPM/60 tokens per second, so a client may
// burst its whole per-minute budget and then sustain QPM.
//
// Over the limit, Slip percent of the queries are answered with an empty
// TC=1 response, so a legitimate client behind a busy prefix retries over
// TCP (never limited: a TCP handshake proves the source address can
// receive), and the rest are dropped without a response, so a spoofed
// flood reflects at most Slip percent tiny packets. DoT, DoH, DoQ and DoH3
// are never limited either. The client is the PROXYv2-attributed address
// when dnsdist forwarded the query. Loopback clients, the health name and
// the Exempt prefixes are never charged.
//
// Buckets live in a sharded LRU bounded by MaxBuckets; an evicted bucket
// starts full again when its prefix returns.

// Defaults for RateLimitConfig.
const (
	DefaultSlip       = 50
	DefaultMaxBuckets = 65536
	rateLimitShards   = 64
)

// DefaultIPv4Limits and DefaultIPv6Limits are the queries per minute
// allowed per prefix length.
var (
	DefaultIPv4Limits = map[int]int{32: 600, 24: 6000}
	DefaultIPv6Limits = map[int]int{128: 600, 64: 6000, 56: 12000}
)

// RateLimitConfig configures a RateLimiter.
type RateLimitConfig struct {
	// IPv4 and IPv6 map a prefix length to the queries per minute allowed
	// for each prefix of that length; a QPM of zero disables the level.
	IPv4 map[int]int
	IPv6 map[int]int
	// Slip is the percentage (0-100) of over-limit queries answered with
	// TC=1; the rest are dropped.
	Slip int
	// MaxBuckets bounds the bucket LRU. Default DefaultMaxBuckets.
	MaxBuckets int
	// Exempt prefixes are never limited (loopback always is).
	Exempt []netip.Prefix
	// Now is the clock for tests; nil means the request's receive time.
	Now func() time.Time
}

// Verdict is the limiter's decision for one query.
type Verdict uint8

// Verdicts.
const (
	// VerdictAllow lets the query through.
	VerdictAllow Verdict = iota
	// VerdictSlip answers the query with an empty TC=1 response.
	VerdictSlip
	// VerdictDrop sends nothing.
	VerdictDrop
)

func (v Verdict) String() string {
	switch v {
	case VerdictSlip:
		return "slip"
	case VerdictDrop:
		return "drop"
	}
	return "allow"
}

// DropReason is the kudurru_dns_dropped_total reason for rate-limited drops.
const DropReason = "ratelimit"

type level struct {
	bits  int
	rate  float64 // tokens per second
	burst float64 // bucket capacity
}

type bucket struct {
	key    netip.Prefix
	tokens float64
	last   int64 // UnixNano of the last refill
}

type shard struct {
	mu  sync.Mutex
	m   map[netip.Prefix]*list.Element
	lru list.List // front = most recently used; values are *bucket
	cap int
}

// RateLimiter is the ingress QPM limiter. It is safe for concurrent use.
type RateLimiter struct {
	lim     atomic.Pointer[limits]
	now     func() time.Time
	seed    maphash.Seed
	shards  [rateLimitShards]shard
	buckets atomic.Int64
	over    atomic.Uint64 // over-limit queries seen, drives the slip ratio
	slipped atomic.Uint64 // cumulative decisions, retained across reconfiguration
	hook    atomic.Pointer[func(Verdict)]
}

// limits are the configuration Reconfigure replaces.
type limits struct {
	disabled bool
	v4, v6   []level
	slip     uint64
	exempt   []netip.Prefix
}

func newLimits(cfg RateLimitConfig) (*limits, int, error) {
	if cfg.Slip < 0 || cfg.Slip > 100 {
		return nil, 0, fmt.Errorf("ingress: rate limit slip %d is outside 0-100", cfg.Slip)
	}
	maxBuckets := cfg.MaxBuckets
	if maxBuckets <= 0 {
		maxBuckets = DefaultMaxBuckets
	}
	v4, err := levels(cfg.IPv4, 32)
	if err != nil {
		return nil, 0, fmt.Errorf("ingress: rate limit ipv4: %w", err)
	}
	v6, err := levels(cfg.IPv6, 128)
	if err != nil {
		return nil, 0, fmt.Errorf("ingress: rate limit ipv6: %w", err)
	}
	return &limits{v4: v4, v6: v6, slip: uint64(cfg.Slip), exempt: cfg.Exempt}, max(maxBuckets/rateLimitShards, 1), nil // #nosec G115 -- slip is 0-100
}

// NewRateLimiter validates cfg and returns a limiter.
func NewRateLimiter(cfg RateLimitConfig) (*RateLimiter, error) {
	lim, per, err := newLimits(cfg)
	if err != nil {
		return nil, err
	}
	l := &RateLimiter{now: cfg.Now, seed: maphash.MakeSeed()}
	l.lim.Store(lim)
	for i := range l.shards {
		l.shards[i].m = make(map[netip.Prefix]*list.Element)
		l.shards[i].cap = per
	}
	return l, nil
}

// Reconfigure replaces the limits (settings hot reload); nil disables
// limiting. Every bucket is dropped, so each prefix starts again with a
// full minute's allowance, as after an LRU eviction. cfg.Now is ignored.
func (l *RateLimiter) Reconfigure(cfg *RateLimitConfig) error {
	lim, per := &limits{disabled: true}, 0
	if cfg != nil {
		var err error
		if lim, per, err = newLimits(*cfg); err != nil {
			return err
		}
	}
	l.lim.Store(lim)
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		n := len(sh.m)
		sh.m = make(map[netip.Prefix]*list.Element)
		sh.lru.Init()
		if per > 0 {
			sh.cap = per
		}
		sh.mu.Unlock()
		l.buckets.Add(int64(-n))
	}
	return nil
}

// Enabled reports whether queries are limited (false after
// Reconfigure(nil)).
func (l *RateLimiter) Enabled() bool { return !l.lim.Load().disabled }

// levels turns a prefix-length → QPM map into levels, most specific first.
func levels(m map[int]int, maxBits int) ([]level, error) {
	var out []level
	for bits, qpm := range m {
		if bits < 0 || bits > maxBits {
			return nil, fmt.Errorf("prefix length %d is outside 0-%d", bits, maxBits)
		}
		if qpm < 0 {
			return nil, fmt.Errorf("/%d: queries per minute %d is negative", bits, qpm)
		}
		if qpm == 0 {
			continue
		}
		out = append(out, level{bits: bits, rate: float64(qpm) / 60, burst: float64(qpm)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].bits > out[j].bits })
	return out, nil
}

// SetHook registers fn to be called synchronously for every slipped or
// dropped query (never for allowed ones); serve counts drops into
// kudurru_dns_dropped_total{reason="ratelimit"}. fn must not block.
func (l *RateLimiter) SetHook(fn func(Verdict)) {
	if fn == nil {
		l.hook.Store(nil)
		return
	}
	l.hook.Store(&fn)
}

// Buckets returns the number of live buckets (kudurru_ratelimit_buckets).
func (l *RateLimiter) Buckets() int { return int(l.buckets.Load()) }

// Collector returns the kudurru_ratelimit_buckets gauge for registration.
func (l *RateLimiter) Collector() prometheus.Collector {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kudurru_ratelimit_buckets",
		Help: "Token buckets currently tracked by the ingress per-prefix QPM limiter.",
	}, func() float64 { return float64(l.Buckets()) })
}

// SlipCollector counts truncated-response decisions independently from drops.
// No client or prefix labels are exported.
func (l *RateLimiter) SlipCollector() prometheus.Collector {
	return prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "kudurru_ratelimit_slipped_total",
		Help: "UDP queries selected for a truncated rate-limit response so clients can retry over TCP.",
	}, func() float64 { return float64(l.slipped.Load()) })
}

// Exempt reports whether addr is never limited.
func (l *RateLimiter) Exempt(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() {
		return true
	}
	for _, p := range l.lim.Load().exempt {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Allow charges one query from addr at now and returns the verdict.
// Callers exempt TCP, loopback and the health name before calling (Check
// does all of it).
func (l *RateLimiter) Allow(addr netip.Addr, now time.Time) Verdict {
	lim := l.lim.Load()
	if lim.disabled {
		return VerdictAllow
	}
	addr = addr.Unmap()
	lv := lim.v4
	if addr.Is6() {
		lv = lim.v6
	}
	ns := now.UnixNano()
	for _, lev := range lv {
		p, err := addr.Prefix(lev.bits)
		if err != nil {
			continue
		}
		if !l.take(p, lev, ns) {
			return l.overLimit(lim.slip)
		}
	}
	return VerdictAllow
}

// overLimit spreads Slip percent of the over-limit queries evenly over the
// sequence: query n slips when floor(n*slip/100) moves.
func (l *RateLimiter) overLimit(slip uint64) Verdict {
	n := l.over.Add(1)
	v := VerdictDrop
	if n*slip/100 != (n-1)*slip/100 {
		v = VerdictSlip
		l.slipped.Add(1)
	}
	if fn := l.hook.Load(); fn != nil {
		(*fn)(v)
	}
	return v
}

// take refills p's bucket and consumes one token if available.
func (l *RateLimiter) take(p netip.Prefix, lev level, ns int64) bool {
	sh := &l.shards[l.shardOf(p)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.m[p]; ok {
		sh.lru.MoveToFront(el)
		b := el.Value.(*bucket)
		if dt := ns - b.last; dt > 0 {
			b.tokens = min(lev.burst, b.tokens+float64(dt)/1e9*lev.rate)
			b.last = ns
		}
		if b.tokens < 1 {
			return false
		}
		b.tokens--
		return true
	}
	if sh.lru.Len() >= sh.cap {
		if back := sh.lru.Back(); back != nil {
			delete(sh.m, back.Value.(*bucket).key)
			sh.lru.Remove(back)
			l.buckets.Add(-1)
		}
	}
	sh.m[p] = sh.lru.PushFront(&bucket{key: p, tokens: lev.burst - 1, last: ns})
	l.buckets.Add(1)
	return true
}

func (l *RateLimiter) shardOf(p netip.Prefix) int {
	var h maphash.Hash
	h.SetSeed(l.seed)
	b := p.Addr().As16()
	_, _ = h.Write(b[:])
	_ = h.WriteByte(byte(p.Bits())) // #nosec G115 -- 0-128
	return int(h.Sum64() % rateLimitShards)
}

// RateLimitFromConfig converts the ratelimit section of kudurru.yaml. It
// returns a nil config (no limiter) when ratelimit.enabled is false.
func RateLimitFromConfig(c config.RateLimit) (*RateLimitConfig, error) {
	if !c.Enabled {
		return nil, nil
	}
	out := &RateLimitConfig{Slip: c.Slip, MaxBuckets: c.MaxBuckets}
	var errs []error
	conv := func(family string, m map[string]int) map[int]int {
		r := make(map[int]int, len(m))
		for k, v := range m {
			bits, err := strconv.Atoi(k)
			if err != nil {
				errs = append(errs, fmt.Errorf("ratelimit.%s: prefix length %q is not a number", family, k))
				continue
			}
			r[bits] = v
		}
		return r
	}
	out.IPv4 = conv("ipv4", c.IPv4)
	out.IPv6 = conv("ipv6", c.IPv6)
	for _, e := range c.Exempt {
		p, err := parsePrefix(e)
		if err != nil {
			errs = append(errs, fmt.Errorf("ratelimit.exempt: %w", err))
			continue
		}
		out.Exempt = append(out.Exempt, p)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// parsePrefix accepts a CIDR or a bare address (a host prefix).
func parsePrefix(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is neither a CIDR nor an address", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}
