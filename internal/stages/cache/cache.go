// Package cache is the response-cache pipeline stage (docs/architecture.md
// §3.3 "internal/cache", §7.1 step 9). It sits after blocking and before
// dnssec/upstream, so blocked and locally answered queries never reach it,
// and it only stores responses the later stages mark Cacheable (forwarded
// through the default group; never local, authoritative or conditional
// answers).
//
// On a fresh hit it answers with TTLs counted down (status cached) and, when
// the entry is hot and about to expire, starts a background refresh (status
// prefetch_refresh for the hit that triggered it). On a miss, concurrent
// identical queries share one upstream fetch. On a hit past expiry it
// refreshes and, when every upstream fails or the refresh takes longer than
// serveStale.maxWait, answers the stale data with serveStale.answerTTL and
// EDE 3 (status cached_stale; RFC 8767).
package cache

import (
	"context"
	"math"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sync/singleflight"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Settings are the DNS-level cache rules.
type Settings struct {
	// MinTTL and MaxTTL clamp positive TTLs (MaxTTL 0: no upper clamp).
	MinTTL, MaxTTL time.Duration
	// NegativeTTL caps NXDOMAIN/NODATA TTLs taken from the SOA (RFC 2308);
	// a negative answer without SOA is kept for min(MinTTL, NegativeTTL).
	NegativeTTL time.Duration
	// StaleAnswerTTL is the TTL of stale answers and how long a failed
	// refresh makes later queries skip the upstream wait (default 30 s).
	StaleAnswerTTL time.Duration
	// StaleMaxWait is how long a query for an expired entry waits for the
	// refresh before the stale answer is sent (default 1.8 s).
	StaleMaxWait time.Duration
	// FetchTimeout bounds one upstream fetch, which runs detached from the
	// client that triggered it (default 5 s).
	FetchTimeout time.Duration
}

// SettingsFrom maps the cache settings.
func SettingsFrom(c config.Cache) Settings {
	return Settings{
		MinTTL:         c.MinTTL.Std(),
		MaxTTL:         c.MaxTTL.Std(),
		NegativeTTL:    c.NegativeTTL.Std(),
		StaleAnswerTTL: c.ServeStale.AnswerTTL.Std(),
		StaleMaxWait:   c.ServeStale.MaxWait.Std(),
	}
}

func (s Settings) withDefaults() Settings {
	if s.StaleAnswerTTL <= 0 {
		s.StaleAnswerTTL = 30 * time.Second
	}
	if s.StaleMaxWait <= 0 {
		s.StaleMaxWait = 1800 * time.Millisecond
	}
	if s.FetchTimeout <= 0 {
		s.FetchTimeout = 5 * time.Second
	}
	return s
}

// Subnetter reports the ECS subnet the upstream stage sends for a query,
// so entries are keyed by it. *upstream.Set and the upstream stage
// implement it.
type Subnetter interface {
	ECSSubnet(m *dns.Msg, client netip.Addr) netip.Prefix
}

// Stage implements pipeline.Stage.
type Stage struct {
	c   *cache.Cache
	set Settings
	ecs Subnetter
	bg  sync.WaitGroup
}

// New returns the stage over c. ecs may be nil when ECS is never sent.
func New(c *cache.Cache, s Settings, ecs Subnetter) *Stage {
	return &Stage{c: c, set: s.withDefaults(), ecs: ecs}
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "cache" }

// Cache returns the underlying cache (Dump/Load, Run, Flush).
func (s *Stage) Cache() *cache.Cache { return s.c }

// Wait blocks until background prefetches have finished.
func (s *Stage) Wait() { s.bg.Wait() }

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	// A forward-zone hint routes the query to a group of its own; its
	// answers are never cached, and an entry stored for the name before the
	// zone existed must not shadow it.
	if req.UpstreamGroup != "" || !cacheable(req.Msg) {
		return next(req)
	}
	k, ecsBits := s.key(req)
	l, ok := s.c.Get(k)
	if ok && l.State == cache.Fresh {
		if resp := s.answer(req, k, l, ecsBits, false); resp != nil {
			if l.Prefetch {
				s.prefetch(k, req, next)
				resp.Status = pipeline.StatusPrefetchRefresh
			}
			return resp, nil
		}
		ok = false
	}
	if ok {
		return s.serveStale(k, req, next, l, ecsBits)
	}
	return s.wait(req, s.fetch(k, req, next))
}

// cacheable reports whether the query can be answered from or stored in
// the cache: one IN question of an ordinary type in a standard query.
func cacheable(m *dns.Msg) bool {
	if m == nil || m.Opcode != dns.OpcodeQuery || len(m.Question) != 1 {
		return false
	}
	q := m.Question[0]
	if q.Qclass != dns.ClassINET {
		return false
	}
	switch q.Qtype {
	case dns.TypeANY, dns.TypeAXFR, dns.TypeIXFR, dns.TypeOPT:
		return false
	}
	return true
}

// key builds the cache key and returns the ECS prefix length in it (-1
// without ECS).
func (s *Stage) key(req *pipeline.Request) (cache.Key, int) {
	m := req.Msg
	q := m.Question[0]
	var do bool
	if opt := m.IsEdns0(); opt != nil {
		do = opt.Do()
	}
	var ecs netip.Prefix
	bits := -1
	if s.ecs != nil {
		if ecs = s.ecs.ECSSubnet(m, req.Client.Addr.Addr()); ecs.IsValid() {
			bits = ecs.Bits()
		}
	}
	return cache.MakeKey(q.Name, q.Qtype, q.Qclass, do, m.CheckingDisabled, ecs), bits
}

// fetch starts (or joins) the coalesced upstream fetch for k. The request
// is copied first: the fetch outlives the client that started it.
func (s *Stage) fetch(k cache.Key, req *pipeline.Request, next pipeline.Next) <-chan singleflight.Result {
	fr := *req
	fr.Msg = req.Msg.Copy()
	parent := req.Ctx
	if parent == nil {
		parent = context.Background()
	}
	return s.c.Coalesce(k, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.set.FetchTimeout)
		defer cancel()
		fr.Ctx = ctx
		fr.Deadline, _ = ctx.Deadline()
		resp, err := next(&fr)
		if refreshed(resp, err) {
			s.store(k, resp)
		} else {
			s.c.MarkRefreshFailed(k, s.set.StaleAnswerTTL)
		}
		return resp, err
	})
}

// refreshed reports whether a fetch produced an answer (as opposed to every
// upstream failing).
func refreshed(resp *pipeline.Response, err error) bool {
	if err != nil || resp == nil || resp.Msg == nil {
		return false
	}
	return resp.Status != pipeline.StatusServfailUpstream && resp.Status != pipeline.StatusTimeoutUpstream
}

// wait delivers a fetch result to req, or gives up when the client does.
func (s *Stage) wait(req *pipeline.Request, ch <-chan singleflight.Result) (*pipeline.Response, error) {
	var done <-chan struct{}
	if req.Ctx != nil {
		done = req.Ctx.Done()
	}
	select {
	case r := <-ch:
		return personalise(req, r)
	case <-done:
		return nil, req.Ctx.Err()
	}
}

// personalise copies a shared fetch result for req: its ID, question
// spelling and EDNS.
func personalise(req *pipeline.Request, r singleflight.Result) (*pipeline.Response, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	resp, _ := r.Val.(*pipeline.Response)
	if resp == nil || resp.Msg == nil {
		return resp, nil
	}
	out := *resp
	out.Msg = resp.Msg.Copy()
	upstream.FinishReply(req.Msg, out.Msg, -1)
	// The cache stage already stored what was cacheable.
	out.Cacheable = false
	return &out, nil
}

// serveStale handles a hit past expiry (RFC 8767).
func (s *Stage) serveStale(k cache.Key, req *pipeline.Request, next pipeline.Next, l cache.Lookup, ecsBits int) (*pipeline.Response, error) {
	if !l.Hold {
		ch := s.fetch(k, req, next)
		timer := time.NewTimer(s.set.StaleMaxWait)
		defer timer.Stop()
		var done <-chan struct{}
		if req.Ctx != nil {
			done = req.Ctx.Done()
		}
		select {
		case r := <-ch:
			if resp, _ := r.Val.(*pipeline.Response); refreshed(resp, r.Err) {
				return personalise(req, r)
			}
		case <-timer.C:
		case <-done:
		}
	}
	resp := s.answer(req, k, l, ecsBits, true)
	if resp == nil {
		return s.wait(req, s.fetch(k, req, next))
	}
	s.c.CountStaleHit(l.Kind)
	return resp, nil
}

// prefetch refreshes k in the background.
func (s *Stage) prefetch(k cache.Key, req *pipeline.Request, next pipeline.Next) {
	s.c.CountPrefetch()
	ch := s.fetch(k, req, next)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		<-ch
		// A successful refresh replaced the entry (and its claim); a failed
		// one must give the claim back.
		s.c.ReleasePrefetch(k)
	}()
}

// answer builds the reply from a cache entry, or nil (after dropping the
// entry) when it does not decode.
func (s *Stage) answer(req *pipeline.Request, k cache.Key, l cache.Lookup, ecsBits int, stale bool) *pipeline.Response {
	m := new(dns.Msg)
	if err := m.Unpack(l.Wire); err != nil {
		s.c.Delete(k)
		return nil
	}
	age := uint32(l.Age / time.Second) //nolint:gosec // non-negative, bounded by the stale window
	staleTTL := seconds(s.set.StaleAnswerTTL)
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			h := rr.Header()
			switch {
			case stale:
				h.Ttl = staleTTL
			case h.Ttl > age:
				h.Ttl -= age
			default:
				h.Ttl = 0
			}
		}
	}
	m.Response = true
	m.Authoritative = false
	m.RecursionAvailable = true
	upstream.FinishReply(req.Msg, m, ecsBits)
	status := pipeline.StatusCached
	if stale {
		status = pipeline.StatusCachedStale
		upstream.AddEDE(m, dns.ExtendedErrorCodeStaleAnswer, "")
	}
	return &pipeline.Response{Msg: m, Status: status}
}

// store packs a cacheable response without its OPT record, TTLs clamped.
func (s *Stage) store(k cache.Key, resp *pipeline.Response) {
	if !resp.Cacheable || resp.Msg == nil || resp.Msg.Truncated {
		return
	}
	m := resp.Msg
	var kind cache.Kind
	switch {
	case m.Rcode == dns.RcodeSuccess && len(m.Answer) > 0:
		kind = cache.Positive
	case m.Rcode == dns.RcodeSuccess || m.Rcode == dns.RcodeNameError:
		kind = cache.Negative
	default:
		return
	}
	out := &dns.Msg{MsgHdr: m.MsgHdr, Compress: true}
	out.Id = 0
	out.Question = append([]dns.Question(nil), m.Question...)
	out.Answer = copyRRs(m.Answer)
	out.Ns = copyRRs(m.Ns)
	out.Extra = copyRRs(m.Extra) // OPT dropped
	var ttl time.Duration
	if kind == cache.Positive {
		ttl = s.clampPositive(out)
	} else {
		ttl = s.clampNegative(out)
	}
	if ttl <= 0 {
		return
	}
	wire, err := out.Pack()
	if err != nil {
		return
	}
	s.c.Set(k, wire, kind, ttl)
}

// seconds converts d to whole seconds for a TTL field, saturating at the
// uint32 range.
func seconds(d time.Duration) uint32 {
	s := d / time.Second
	switch {
	case s <= 0:
		return 0
	case s >= math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(s)
}

func copyRRs(in []dns.RR) []dns.RR {
	if len(in) == 0 {
		return nil
	}
	out := make([]dns.RR, 0, len(in))
	for _, rr := range in {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		out = append(out, dns.Copy(rr))
	}
	return out
}

// clampPositive clamps every TTL into [MinTTL, MaxTTL] and returns the
// entry lifetime: the smallest clamped TTL.
func (s *Stage) clampPositive(m *dns.Msg) time.Duration {
	lo := seconds(s.set.MinTTL)
	hi := seconds(s.set.MaxTTL)
	least := uint32(0)
	first := true
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			h := rr.Header()
			t := max(h.Ttl, lo)
			if hi > 0 {
				t = min(t, hi)
			}
			h.Ttl = t
			if first || t < least {
				least, first = t, false
			}
		}
	}
	return time.Duration(least) * time.Second
}

// clampNegative applies RFC 2308: the lifetime is min(SOA TTL, SOA
// minimum) capped by NegativeTTL; without a SOA, min(MinTTL, NegativeTTL).
// No record outlives the entry.
func (s *Stage) clampNegative(m *dns.Msg) time.Duration {
	limit := seconds(s.set.NegativeTTL)
	var ttl uint32
	found := false
	for _, rr := range m.Ns {
		if soa, ok := rr.(*dns.SOA); ok {
			ttl, found = min(soa.Hdr.Ttl, soa.Minttl), true
			break
		}
	}
	if !found {
		ttl = seconds(s.set.MinTTL)
	}
	if limit > 0 {
		ttl = min(ttl, limit)
	}
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			rr.Header().Ttl = min(rr.Header().Ttl, ttl)
			if _, ok := rr.(*dns.SOA); ok {
				rr.Header().Ttl = ttl
			}
		}
	}
	return time.Duration(ttl) * time.Second
}
