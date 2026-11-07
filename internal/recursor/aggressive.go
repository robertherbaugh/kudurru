package recursor

import (
	"container/list"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// RFC 8198 proof storage is separate from the delegation cache: its minimum
// five-second TTL would extend short-lived authenticated denial evidence.
const (
	maxDenialEntries    = 1024
	maxDenialBytes      = 4 << 20 // retained encoded records and index keys
	maxDenialRecords    = 48
	maxDenialProofBytes = 16 << 10
	maxDenialPerZone    = 16
	maxDenialCandidates = 32
	maxDenialTTL        = 3 * time.Hour
)

// denialProof contains only RRsets validated under one signer, with the actual
// verifying signatures. After admission all its records are private copies.
// Bundles are never combined across responses, signers or NSEC3 parameters.
type denialProof struct {
	zone      string
	observed  time.Time
	until     time.Time
	authority []dns.RR
	nsecs     []*dns.NSEC
	nsec3s    []*dns.NSEC3
	soa       bool
	wildcard  string // closest encloser of records, empty for negative bundles
	records   []dns.RR
}

type denialEntry struct {
	key   string
	proof *denialProof
	size  int
}

type denialCache struct {
	mu      sync.Mutex
	epoch   uint64
	bytes   int
	entries *list.List
	zones   map[string][]*list.Element
}

func newDenialCache() *denialCache {
	return &denialCache{entries: list.New(), zones: make(map[string][]*list.Element)}
}

func (c *denialCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

func (c *denialCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.bytes = 0
	c.entries.Init()
	c.zones = make(map[string][]*list.Element)
}

func (c *denialCache) remove(e *list.Element) {
	it := e.Value.(*denialEntry)
	zone := c.zones[it.key]
	for i, candidate := range zone {
		if candidate == e {
			copy(zone[i:], zone[i+1:])
			zone[len(zone)-1] = nil
			zone = zone[:len(zone)-1]
			break
		}
	}
	if len(zone) == 0 {
		delete(c.zones, it.key)
	} else {
		c.zones[it.key] = zone
	}
	c.bytes -= it.size
	c.entries.Remove(e)
}

func (c *denialCache) put(key string, epoch uint64, p *denialProof, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return // a query captured before Flush cannot refill its successor
	}
	if zone := c.zones[key]; len(zone) >= maxDenialPerZone {
		c.remove(zone[0])
	}
	e := c.entries.PushFront(&denialEntry{key: key, proof: p, size: size})
	c.zones[key] = append(c.zones[key], e)
	c.bytes += size
	for c.entries.Len() > maxDenialEntries || c.bytes > maxDenialBytes {
		c.remove(c.entries.Back())
	}
}

func (c *denialCache) candidates(key string, epoch uint64, now time.Time, limit int) []*denialProof {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return nil
	}
	var out []*denialProof
	// Removal changes the zone slice, so walk a bounded copy of its pointers.
	zone := append([]*list.Element(nil), c.zones[key]...)
	for i := len(zone) - 1; i >= 0 && len(out) < limit; i-- {
		e := zone[i]
		p := e.Value.(*denialEntry).proof
		if now.Before(p.observed) || !now.Before(p.until) {
			c.remove(e)
			continue
		}
		c.entries.MoveToFront(e)
		out = append(out, p)
	}
	return out
}

// rememberDenial accepts a complete validated negative proof, or a validated
// wildcard with its exclusion proof. It never saves a synthesized response.
func (q *query) rememberDenial(p *denialProof) {
	if p == nil || !q.validate || q.st.noSynthesis || q.limited() ||
		(!p.soa && len(p.records) == 0) || len(p.nsecs)+len(p.nsec3s) == 0 ||
		len(p.authority)+len(p.records) > maxDenialRecords {
		return
	}
	// Mixed chains and opt-out are conservatively left to normal recursion.
	if len(p.nsecs) > 0 && len(p.nsec3s) > 0 {
		return
	}
	for _, n := range p.nsec3s {
		first := p.nsec3s[0]
		if n.Flags != 0 || n.Hash != dns.SHA1 || n.Iterations > maxNSEC3Iter ||
			n.Iterations != first.Iterations || n.Salt != first.Salt {
			return
		}
	}
	copyProof := &denialProof{zone: p.zone, observed: p.observed, until: p.observed.Add(maxDenialTTL), soa: p.soa, wildcard: p.wildcard}
	size := 0
	for i, section := range [][]dns.RR{p.authority, p.records} {
		for _, rr := range section {
			size += dns.Len(rr)
			if size > maxDenialProofBytes {
				return
			}
			until := p.observed.Add(time.Duration(rr.Header().Ttl) * time.Second)
			if s, ok := rr.(*dns.RRSIG); ok {
				if !s.ValidityPeriod(p.observed) {
					return
				}
				until = earlier(until, time.Unix(int64(s.Expiration), 0))
				until = earlier(until, p.observed.Add(time.Duration(s.OrigTtl)*time.Second))
			}
			if s, ok := rr.(*dns.SOA); ok {
				until = earlier(until, p.observed.Add(time.Duration(s.Minttl)*time.Second))
			}
			copyProof.until = earlier(copyProof.until, until)
			r := dns.Copy(rr)
			if i == 1 {
				r.Header().Name = wildcardOf(p.wildcard)
				copyProof.records = append(copyProof.records, r)
				continue
			}
			copyProof.authority = append(copyProof.authority, r)
			switch n := r.(type) {
			case *dns.NSEC:
				copyProof.nsecs = append(copyProof.nsecs, n)
			case *dns.NSEC3:
				copyProof.nsec3s = append(copyProof.nsec3s, n)
			}
		}
	}
	if !q.st.exceptionUntil.IsZero() {
		copyProof.until = earlier(copyProof.until, q.st.exceptionUntil)
	}
	now := q.now()
	if now.Before(p.observed) || now.Before(q.st.observed) || !now.Before(copyProof.until) || q.ctx.Err() != nil {
		return
	}
	key := q.key("denial|", p.zone)
	q.r.denials.put(key, q.st.denialEpoch, copyProof, size+len(key))
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (p *denialProof) synthesize(name string, typ uint16, budget *int) *answer {
	if len(p.records) > 0 {
		if typ != p.records[0].Header().Rrtype && p.records[0].Header().Rrtype != dns.TypeCNAME {
			return nil
		}
		if !isBelow(p.wildcard, name) || !wildcardProof(name, p.wildcard, p.zone, p.nsecs, p.nsec3s, budget, true) {
			return nil
		}
		return &answer{sec: SecSecure, records: p.records, proofs: p.authority}
	}
	if !p.soa {
		return nil
	}
	// DS belongs to the parent; an apex denial cannot speak for its own DS.
	if typ == dns.TypeDS && name != "." && !isBelow(p.zone, name) {
		return nil
	}
	if len(p.nsecs) > 0 {
		if ok, _ := nsecProveNoData(name, typ, p.nsecs); ok {
			return &answer{sec: SecSecure, authority: p.authority}
		}
		if nsecProveNX(name, p.nsecs) {
			return &answer{sec: SecSecure, rcode: dns.RcodeNameError, authority: p.authority}
		}
		return nil
	}
	s, insecure := newNSEC3Set(p.nsec3s, p.zone, budget)
	if s == nil || insecure {
		return nil
	}
	if ok, _, optout := s.proveNoData(name, typ); ok && !optout {
		return &answer{sec: SecSecure, authority: p.authority}
	}
	if ok, optout := s.proveNX(name); ok && !optout {
		return &answer{sec: SecSecure, rcode: dns.RcodeNameError, authority: p.authority}
	}
	return nil
}

func (q *query) cachedDenial(name string, typ uint16) *answer {
	// Meta-queries do not ask whether a single RR type is absent.
	if typ >= dns.TypeIXFR && typ <= dns.TypeANY || typ == 0 {
		return nil
	}
	if !q.validate || q.st.noSynthesis || q.st.synthesisBudget <= 0 || q.limited() || q.exception(name) {
		return nil
	}
	now := q.now()
	if now.Before(q.st.observed) || now.Before(q.st.denialObserved) {
		return nil
	}
	remaining := maxDenialCandidates
	budget := min(q.st.hashBudget/2, q.st.synthesisBudget)
	for zone := name; ; zone = parentName(zone) {
		for _, p := range q.r.denials.candidates(q.key("denial|", zone), q.st.denialEpoch, now, remaining) {
			remaining--
			// Use a private portion of the original budget. A miss does not
			// starve ordinary validation, but its hash work still counts.
			before := budget
			a := p.synthesize(name, typ, &budget)
			q.st.hashBudget -= before - budget
			q.st.synthesisBudget -= before - budget
			if a == nil {
				if q.st.synthesisBudget == 0 {
					return nil
				}
				continue
			}
			final := q.now()
			if q.ctx.Err() != nil || final.Before(now) || !final.Before(p.until) {
				return nil
			}
			left := uint32(p.until.Sub(final) / time.Second) //nolint:gosec // positive duration bounded by maxDenialTTL at admission
			if left == 0 {
				return nil
			}
			copyRecords := func(rrs []dns.RR, expand bool) []dns.RR {
				out := make([]dns.RR, 0, len(rrs))
				for _, rr := range rrs {
					r := dns.Copy(rr)
					r.Header().Ttl = min(r.Header().Ttl, left)
					if expand {
						r.Header().Name = name
					}
					out = append(out, r)
				}
				return out
			}
			a.records = copyRecords(a.records, true)
			a.authority = copyRecords(a.authority, false)
			a.proofs = copyRecords(a.proofs, false)
			if q.st.denialUntil.IsZero() || p.until.Before(q.st.denialUntil) {
				q.st.denialUntil = p.until
			}
			q.st.denialObserved = final
			return a
		}
		if zone == "." || remaining == 0 {
			return nil
		}
	}
}
