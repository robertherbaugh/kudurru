package responsepolicy

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// RewriteSpec declares an exact-name or descendant-only wildcard rewrite.
// CNAME is a convenience alternative to Type CNAME with one target. An empty
// Type accepts only IPv4/IPv6 targets and infers each record's address family.
type RewriteSpec struct {
	Name, Type, CNAME string
	Targets           []string
}

// Rewrite is an immutable compiled rule.
type Rewrite struct {
	name    string
	wild    bool
	records []dns.RR
}

// CompileRewrites rejects invalid records before publication. Each rule has a
// bounded set of presentation-format RDATA values; no names are resolved here.
func CompileRewrites(specs []RewriteSpec) ([]*Rewrite, error) {
	if len(specs) > 128 {
		return nil, fmt.Errorf("at most 128 rewrites per policy")
	}
	out := make([]*Rewrite, 0, len(specs))
	for i, spec := range specs {
		wild := strings.HasPrefix(spec.Name, "*.")
		name := strings.TrimPrefix(spec.Name, "*.")
		if !validName(name) || name == "." || strings.Contains(name, "*") {
			return nil, fmt.Errorf("rewrites[%d]: invalid exact name or *.suffix", i)
		}
		rule := &Rewrite{name: dns.CanonicalName(name), wild: wild}
		typ, targets := strings.ToUpper(spec.Type), spec.Targets
		if spec.CNAME != "" {
			if len(targets) != 0 || typ != "" && typ != "CNAME" {
				return nil, fmt.Errorf("rewrites[%d]: CNAME cannot be combined with other targets or types", i)
			}
			typ, targets = "CNAME", []string{spec.CNAME}
		}
		if len(targets) == 0 || len(targets) > 16 || typ == "CNAME" && len(targets) != 1 {
			return nil, fmt.Errorf("rewrites[%d]: expected 1 to 16 targets (one for CNAME)", i)
		}
		switch typ {
		case "", "A", "AAAA", "CNAME", "PTR", "TXT", "MX", "SRV", "HTTPS", "SVCB", "CAA", "NS", "NAPTR":
		default:
			return nil, fmt.Errorf("rewrites[%d]: unsupported record type %q", i, typ)
		}
		for _, value := range targets {
			if len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("rewrites[%d]: targets must be single-line RDATA", i)
			}
			t := typ
			if t == "" {
				ip, err := netip.ParseAddr(value)
				if err != nil || ip.Zone() != "" {
					return nil, fmt.Errorf("rewrites[%d]: specify a type for non-address targets", i)
				}
				t = "AAAA"
				if ip.Is4() {
					t = "A"
				}
			}
			rr, err := dns.NewRR(rule.name + " 0 IN " + t + " " + value)
			if err != nil || rr == nil {
				return nil, fmt.Errorf("rewrites[%d]: invalid %s target", i, t)
			}
			if c, ok := rr.(*dns.CNAME); ok && !wild && strings.EqualFold(c.Target, rule.name) {
				return nil, fmt.Errorf("rewrites[%d]: self-referential CNAME", i)
			}
			rule.records = append(rule.records, rr)
		}
		out = append(out, rule)
	}
	// Policies remain ordered by priority outside this function. Inside each
	// policy an exact rule wins over its enclosing wildcard; ties retain order.
	slices.SortStableFunc(out, func(a, b *Rewrite) int {
		if n := dns.CountLabel(b.name) - dns.CountLabel(a.name); n != 0 {
			return n
		}
		if a.wild != b.wild {
			if a.wild {
				return 1
			}
			return -1
		}
		return 0
	})
	return out, nil
}

// RewriteAnswer returns the first applicable policy rule, or nil. Requested
// families get only their own records; other types get NODATA. CNAME rules
// return an alias for every ordinary type and preserve authority boundaries
// when the client follows that alias with a new query.
func (s *Settings) RewriteAnswer(req *pipeline.Request, rules []*Rewrite) *pipeline.Response {
	if req.Question().Qclass != dns.ClassINET {
		return nil
	}
	name, typ := dns.CanonicalName(req.Question().Name), req.Question().Qtype
	for _, rule := range rules {
		match := name == rule.name
		if rule.wild {
			match = name != rule.name && dns.IsSubDomain(rule.name, name)
		}
		if !match {
			continue
		}
		ttl := uint32(300)
		if s != nil {
			ttl = s.ttl
		}
		out := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord)
		out.Origin, out.Modified, out.Rule = pipeline.OriginSynthetic, true, "policy_rewrite:"+rule.name
		out.Msg.RecursionAvailable = true
		for _, rr := range rule.records {
			if rr.Header().Rrtype != typ && rr.Header().Rrtype != dns.TypeCNAME {
				continue
			}
			r := dns.Copy(rr)
			r.Header().Name, r.Header().Ttl = req.Question().Name, ttl
			out.Msg.Answer = append(out.Msg.Answer, r)
		}
		return s.filterIPv6(out)
	}
	return nil
}
