// Package rpz compiles complete transferred response policy zones into
// immutable query-time decisions. Its background manager acquires and persists
// feeds; query-time selection never performs network or store calls. Rendering
// belongs to the serving adapter.
package rpz

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Action is the policy encoded by one owner RRset.
type Action string

// Supported RPZ actions. The zero value means no policy matched.
const (
	ActionNone      Action = ""
	ActionPassthru  Action = "passthru"
	ActionDrop      Action = "drop"
	ActionTCPOnly   Action = "tcp-only"
	ActionNXDOMAIN  Action = "nxdomain"
	ActionNODATA    Action = "nodata"
	ActionLocalData Action = "local-data"
)

// Trigger identifies the input matched by a policy owner.
type Trigger string

// Supported triggers. Resolver NS provenance is unavailable to the forwarder,
// so NSDNAME and NSIP owners are diagnosed rather than compiled.
const (
	TriggerQName      Trigger = "qname"
	TriggerClientIP   Trigger = "client-ip"
	TriggerResponseIP Trigger = "response-ip"
)

// Spec binds one policy source to its configuration and absolute lifetime.
// Identity is supplied by the acquisition adapter and contains no secrets.
type Spec struct {
	List, Source, Origin, Identity string
	ExpiresAt                      time.Time
	// Exclude contains canonical absolute policy-owner names to switch off.
	Exclude []string
}

// Context is a complete final-response inspection input. Names holds returned
// CNAME targets in resolution order; Addresses contains answer A/AAAA only.
// All slices remain owned by the caller and are never modified or retained.
type Context struct {
	Name        string
	Type, Class uint16
	ClientIP    netip.Addr
	Names       []string
	Addresses   []netip.Addr
}

// Stats counts owners for policy outcomes and records for raw data. Ignored
// counts metadata and duplicate records omitted from executable RRsets.
type Stats struct {
	Records       int `json:"records"`
	Rules         int `json:"rules"`
	Invalid       int `json:"invalid"`
	Unsupported   int `json:"unsupported"`
	Excluded      int `json:"excluded"`
	Ignored       int `json:"ignored"`
	StaleExcludes int `json:"staleExcludes"`
}

// Diagnostic explains one ignored policy owner. Samples are capped while
// counts and compilation always consume the complete input.
type Diagnostic struct {
	Owner, Reason string
	Unsupported   bool
}

// Entry explains a compiled policy without exposing mutable DNS records.
type Entry struct {
	Owner, Value string
	Trigger      Trigger
	Action       Action
	TTL          uint32
}

type rule struct {
	entry   Entry
	prefix  netip.Prefix
	records []dns.RR
}

// Zone is an immutable, node-local compiled RPZ source.
type Zone struct {
	spec        Spec
	soa         *dns.SOA
	records     []dns.RR
	rules       []*rule
	qnames      map[string]*rule
	nodes       map[string]bool
	clients     map[netip.Prefix]*rule
	addresses   map[netip.Prefix]*rule
	stats       Stats
	diagnostics []Diagnostic
	confirmedAt time.Time
}

// AvailableAt is false for placeholders and at or beyond exact expiry.
func (z *Zone) AvailableAt(now time.Time) bool {
	return z != nil && z.soa != nil && !z.spec.ExpiresAt.IsZero() && now.Before(z.spec.ExpiresAt) &&
		(z.confirmedAt.IsZero() || !now.Before(z.confirmedAt))
}

// SOA returns a private copy for acquisition timers and external serials.
func (z *Zone) SOA() *dns.SOA {
	if z == nil || z.soa == nil {
		return nil
	}
	return dns.Copy(z.soa).(*dns.SOA)
}

// Records returns private raw records with the apex SOA first, suitable for
// an IXFR base. It does not add a closing SOA or alter serials or TTLs.
func (z *Zone) Records() []dns.RR {
	if z == nil {
		return nil
	}
	out := make([]dns.RR, len(z.records))
	for i, rr := range z.records {
		out[i] = dns.Copy(rr)
	}
	return out
}

// Stats returns complete compilation counts.
func (z *Zone) Stats() Stats { return z.stats }

// Diagnostics returns private, bounded diagnostic samples.
func (z *Zone) Diagnostics() []Diagnostic { return slices.Clone(z.diagnostics) }

// Entries returns every compiled owner in canonical owner order, for source
// explanations. Execution never relies on this diagnostic representation.
func (z *Zone) Entries() []Entry {
	out := make([]Entry, len(z.rules))
	for i, r := range z.rules {
		out[i] = r.entry
	}
	return out
}

// Decision identifies one matched policy or an unavailable required source.
// A passthru stops subsequent RPZ sources, not unrelated blocklist engines.
type Decision struct {
	Action                                Action
	Trigger                               Trigger
	List, Source, Identity, Origin, Owner string
	Matched                               string
	Unavailable                           bool
	rule                                  *rule
	expiresAt                             time.Time
	now                                   time.Time
}

// Rcode returns the synthetic action's response code. Drop/TCP-only/passthru
// require transport-aware rendering, not an ordinary empty DNS response.
func (d Decision) Rcode() int {
	if d.Unavailable {
		return dns.RcodeServerFailure
	}
	if d.Action == ActionNXDOMAIN {
		return dns.RcodeNameError
	}
	return dns.RcodeSuccess
}

// Records returns private local-data records for the query type (or CNAME).
// maxTTL=0 leaves the feed TTL uncapped except by the absolute remaining
// lifetime. Wildcard CNAME targets prepend the original query name.
func (d Decision) Records(name string, typ uint16, maxTTL uint32) ([]dns.RR, error) {
	if d.Unavailable || d.Action != ActionLocalData || d.rule == nil {
		return nil, nil
	}
	name, err := canonicalName(name)
	if err != nil {
		return nil, err
	}
	var out []dns.RR
	for _, rr := range d.rule.records {
		if rr.Header().Rrtype != typ && rr.Header().Rrtype != dns.TypeCNAME && typ != dns.TypeANY {
			continue
		}
		copyRR := dns.Copy(rr)
		copyRR.Header().Name = name
		if maxTTL > 0 {
			copyRR.Header().Ttl = min(copyRR.Header().Ttl, maxTTL)
		}
		seconds := max(d.expiresAt.Sub(d.now)/time.Second, 0)
		if seconds < time.Duration(copyRR.Header().Ttl) {
			copyRR.Header().Ttl = uint32(seconds) //nolint:gosec // clamped to a uint32 TTL
		}
		if cname, ok := copyRR.(*dns.CNAME); ok && strings.HasPrefix(cname.Target, "*.") {
			cname.Target, err = canonicalName(name + strings.TrimPrefix(cname.Target, "*."))
			if err != nil {
				return nil, fmt.Errorf("rpz: wildcard CNAME expansion: %w", err)
			}
		}
		out = append(out, copyRR)
	}
	return out, nil
}

// Selection is an immutable ordered list of required, enabled RPZ sources.
type Selection struct{ zones []*Zone }

// NewSelection copies the caller's source order. Nil sources remain unavailable
// sentinels; disabled sources should be omitted by the configuration adapter.
func NewSelection(zones ...*Zone) *Selection { return &Selection{zones: slices.Clone(zones)} }

// Empty reports whether no RPZ sources are required. Unavailable sources,
// including nil sentinels, still count as required coverage.
func (s *Selection) Empty() bool { return s == nil || len(s.zones) == 0 }

// Evaluate checks source availability before selecting the first matching
// feed. Within a feed client-IP precedes QNAME, then answer-IP. Caller guards
// recursive-only/opcode/authority and performs final transport rendering.
func (s *Selection) Evaluate(ctx Context, now time.Time) Decision {
	if s == nil || (ctx.Class != 0 && ctx.Class != dns.ClassINET) {
		return Decision{}
	}
	for _, z := range s.zones {
		if !z.AvailableAt(now) {
			d := Decision{Unavailable: true}
			if z != nil {
				d.List, d.Source, d.Identity, d.Origin = z.spec.List, z.spec.Source, z.spec.Identity, z.spec.Origin
			}
			return d
		}
	}
	for _, z := range s.zones {
		if r, matched := z.match(ctx); r != nil {
			return Decision{Action: r.entry.Action, Trigger: r.entry.Trigger,
				List: z.spec.List, Source: z.spec.Source, Identity: z.spec.Identity, Origin: z.spec.Origin,
				Owner: r.entry.Owner, Matched: matched, rule: r, expiresAt: z.spec.ExpiresAt, now: now}
		}
	}
	return Decision{}
}
