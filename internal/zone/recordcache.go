package zone

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// RecordCache retains parsed record sets, never serving decisions or authority.
// Its zero value is ready for concurrent use. Each owner has its own cache.
// Retention is limited to 4096 objects and 16 MiB of estimated retained data;
// a miss or eviction changes only parsing cost. Inputs must remain immutable
// throughout a call, as for Build. No input object or endpoint is retained.
type RecordCache struct {
	mu      sync.Mutex
	entries map[recordAddress]*list.Element
	lru     list.List
	bytes   int
}

const recordCacheObjects = 4096
const recordCacheBytes = 16 << 20

type parseScope struct {
	namespace, name, uid, origin string
	ttl                          uint32
}
type recordAddress struct{ zoneNamespace, zoneName, namespace, name string }
type parsedEndpoint struct {
	records []dns.RR
	errors  field.ErrorList
}
type recordParse struct {
	endpoints []parsedEndpoint
	valid     bool
}
type recordEntry struct {
	address    recordAddress
	scope      parseScope
	digest     [32]byte
	parsed     *recordParse
	at, expiry time.Time
	bytes      int
}

func recordScope(z *public.DNSZone) parseScope {
	origin := canonicalName(z.Spec.ZoneName)
	if key, err := canonicalKey(origin); err == nil {
		origin = nameFromKey(key)
	}
	return parseScope{z.Namespace, z.Name, string(z.UID), origin, orDefault(z.Spec.DefaultTTL, DefaultTTL)}
}

func parseRecordSet(scope parseScope, rs *public.DNSRecordSet, validate bool) *recordParse {
	out := &recordParse{valid: true, endpoints: make([]parsedEndpoint, len(rs.Spec.Endpoints))}
	if validate {
		out.valid = RecordSetValid(rs)
	}
	for i := range rs.Spec.Endpoints {
		rrs, errs := ParseEndpoint(scope.origin, scope.ttl, &rs.Spec.Endpoints[i], field.NewPath("spec", "endpoints").Index(i))
		out.endpoints[i] = parsedEndpoint{rrs, errs}
	}
	return out
}

// load returns private immutable parsing. Only Records/Add below export copies.
func (c *RecordCache) load(scope parseScope, rs *public.DNSRecordSet, now time.Time) *recordParse {
	if c == nil {
		return parseRecordSet(scope, rs, false)
	}
	data, err := json.Marshal(rs)
	if err != nil || len(data) > recordCacheBytes {
		return parseRecordSet(scope, rs, true)
	}
	digest := sha256.Sum256(data)
	address := recordAddress{scope.namespace, scope.name, rs.Namespace, rs.Name}
	c.mu.Lock()
	if e := c.entries[address]; e != nil {
		entry := e.Value.(*recordEntry)
		if entry.scope == scope && entry.digest == digest && !now.Before(entry.at) && (entry.expiry.IsZero() || now.Before(entry.expiry)) {
			c.lru.MoveToFront(e)
			c.mu.Unlock()
			return entry.parsed
		}
		c.remove(e)
	}
	c.mu.Unlock()
	parsed := parseRecordSet(scope, rs, true)
	if !parsed.valid {
		return parsed
	}
	size := len(data) + 256 + len(parsed.endpoints)*64
	var expiry time.Time
	for i, ep := range parsed.endpoints {
		if len(ep.errors) != 0 {
			return parsed
		}
		for _, rr := range ep.records {
			size += 128 + 2*dns.Len(rr)
		}
		ext := rs.Spec.Endpoints[i].Kudurru
		if ext != nil && ext.ExpiresAt != nil && now.Before(ext.ExpiresAt.Time) && (expiry.IsZero() || ext.ExpiresAt.Time.Before(expiry)) {
			expiry = ext.ExpiresAt.Time
		}
	}
	if size > recordCacheBytes {
		return parsed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[recordAddress]*list.Element)
	}
	// Concurrent old/new builds may replace this optimization entry. Every hit
	// still requires exact identity/content/context; this grants no currency.
	if e := c.entries[address]; e != nil {
		c.remove(e)
	}
	for len(c.entries) >= recordCacheObjects || c.bytes+size > recordCacheBytes {
		c.remove(c.lru.Back())
	}
	entry := &recordEntry{address: address, scope: scope, digest: digest, parsed: parsed, at: now, expiry: expiry, bytes: size}
	c.entries[address] = c.lru.PushFront(entry)
	c.bytes += size
	return parsed
}

func (c *RecordCache) remove(e *list.Element) {
	entry := e.Value.(*recordEntry)
	delete(c.entries, entry.address)
	c.bytes -= entry.bytes
	c.lru.Remove(e)
}

// Prune drops absent objects in this complete zone input. Concurrent builds may
// evict each other's entries but never change each other's immutable results.
// Deletions of entire zones are also eventually bounded by LRU eviction.
func (c *RecordCache) Prune(z *public.DNSZone, sets []*public.DNSRecordSet) {
	if c == nil || z == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	present := make(map[recordAddress]bool, min(len(sets), len(c.entries)))
	for _, rs := range sets {
		if rs == nil {
			continue
		}
		address := recordAddress{z.Namespace, z.Name, rs.Namespace, rs.Name}
		if c.entries[address] != nil {
			present[address] = true
		}
	}
	for address, e := range c.entries {
		if address.zoneNamespace == z.Namespace && address.zoneName == z.Name && !present[address] {
			c.remove(e)
		}
	}
}

// RecordSetValid reuses the same whole-object validation as ServedRecordSets.
func (c *RecordCache) RecordSetValid(z *public.DNSZone, rs *public.DNSRecordSet, now time.Time) bool {
	if c == nil {
		return RecordSetValid(rs)
	}
	return c.load(recordScope(z), rs, now).valid
}

// ParsedRecordSet binds validated parsing to one builder and its immutable
// input. It cannot be fabricated with caller-supplied unchecked records.
type ParsedRecordSet struct {
	builder   *Builder
	endpoints []public.Endpoint
	parsed    *recordParse
}

// ParseRecordSet prepares reusable parsing for one object in this builder.
func (b *Builder) ParseRecordSet(rs *public.DNSRecordSet) *ParsedRecordSet {
	endpoints := make([]public.Endpoint, len(rs.Spec.Endpoints))
	for i := range endpoints {
		rs.Spec.Endpoints[i].DeepCopyInto(&endpoints[i])
	}
	return &ParsedRecordSet{b, endpoints, b.records.load(b.recordScope, rs, b.now)}
}

// Records returns detached RRs and errors for endpoint i, before serving/conflict
// decisions. This includes inactive endpoints used for ownership reservations.
func (p *ParsedRecordSet) Records(i int) ([]dns.RR, field.ErrorList) {
	ep := p.parsed.endpoints[i]
	var records []dns.RR
	if ep.records != nil {
		records = make([]dns.RR, len(ep.records))
	}
	for j, rr := range ep.records {
		records[j] = dns.Copy(rr)
	}
	var errs field.ErrorList
	if ep.errors != nil {
		errs = make(field.ErrorList, len(ep.errors))
	}
	for j, err := range ep.errors {
		copyErr := *err
		errs[j] = &copyErr
	}
	return records, errs
}

// Add applies this builder's unchanged serving, ownership and conflict rules.
func (p *ParsedRecordSet) Add(source string, i int) field.ErrorList {
	path := field.NewPath("spec", "endpoints").Index(i)
	ep := &p.endpoints[i]
	if p.builder.z.typ != public.ZoneTypePrimary || !Served(ep, p.builder.now) {
		return p.builder.Add(source, path, ep)
	}
	records, errs := p.Records(i)
	return p.builder.addParsed(source, path, ep, records, errs)
}
