package rpz

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"github.com/miekg/dns"
)

// Compiler limits agree with the default bounded inbound transfer budget.
const (
	MaxRecords     = 1_000_000
	MaxBytes       = 64 << 20
	MaxDiagnostics = 1000
)

// Compile consumes a complete transferred RR set, without a closing AXFR SOA.
// Nil records creates an unavailable placeholder retaining source identity.
// Structural errors return no candidate; unsupported/invalid policy owners
// are ignored individually and diagnosed without dropping valid rules.
func Compile(spec Spec, records []dns.RR) (*Zone, error) {
	if spec.List == "" || spec.Source == "" || spec.Identity == "" {
		return nil, fmt.Errorf("rpz: list, source and configuration identity are required")
	}
	origin, err := canonicalName(spec.Origin)
	if err != nil || origin == "." {
		return nil, fmt.Errorf("rpz: a valid non-root origin is required")
	}
	spec.Origin = origin
	spec.Exclude = slices.Clone(spec.Exclude)
	excluded := map[string]bool{}
	for i, owner := range spec.Exclude {
		owner, err = canonicalName(owner)
		if err != nil || !dns.IsSubDomain(origin, owner) {
			return nil, fmt.Errorf("rpz: exclusion must be a valid in-zone policy owner")
		}
		spec.Exclude[i], excluded[owner] = owner, false
	}
	z := &Zone{spec: spec, qnames: map[string]*rule{}, nodes: map[string]bool{".": true},
		clients: map[netip.Prefix]*rule{}, addresses: map[netip.Prefix]*rule{}}
	if records == nil {
		return z, nil
	}
	if spec.ExpiresAt.IsZero() || len(records) > MaxRecords {
		return nil, fmt.Errorf("rpz: expiry is required and record count must be bounded")
	}
	owners := map[string][]dns.RR{}
	var bytes int64
	apexNS := false
	for _, input := range records {
		if input == nil || reflect.ValueOf(input).Kind() == reflect.Pointer && reflect.ValueOf(input).IsNil() {
			return nil, fmt.Errorf("rpz: nil transfer record")
		}
		h := input.Header()
		owner, err := canonicalName(h.Name)
		if err != nil || h.Class != dns.ClassINET || !dns.IsSubDomain(origin, owner) || illegalType(h.Rrtype) {
			return nil, fmt.Errorf("rpz: invalid, out-of-zone or meta-type transfer record")
		}
		if factory, known := dns.TypeToRR[h.Rrtype]; known && reflect.TypeOf(factory()) != reflect.TypeOf(input) {
			return nil, fmt.Errorf("rpz: record concrete type does not match its header")
		}
		copyRR := dns.Copy(input)
		size := dns.Len(copyRR)
		if int64(size) > MaxBytes-bytes {
			return nil, fmt.Errorf("rpz: expanded transfer exceeds byte budget")
		}
		wire := make([]byte, size)
		end, err := dns.PackRR(copyRR, wire, 0, nil, false)
		if err != nil {
			return nil, fmt.Errorf("rpz: malformed record: %w", err)
		}
		if _, offset, err := dns.UnpackRR(wire[:end], 0); err != nil || offset != end {
			return nil, fmt.Errorf("rpz: malformed record encoding")
		}
		if copyRR.Header().Rdlength == 0 {
			switch h.Rrtype {
			case dns.TypeNULL, dns.TypeAPL:
			default:
				if _, known := dns.TypeToRR[h.Rrtype]; known {
					return nil, fmt.Errorf("rpz: missing record data")
				}
			}
		}
		bytes += int64(end)
		if bytes > MaxBytes {
			return nil, fmt.Errorf("rpz: expanded transfer exceeds byte budget")
		}
		if soa, ok := copyRR.(*dns.SOA); ok {
			if owner != origin || z.soa != nil || soa.Ns == "" || soa.Mbox == "" {
				return nil, fmt.Errorf("rpz: exactly one valid apex SOA is required, without closing SOA")
			}
			z.soa = soa
		}
		if owner == origin && h.Rrtype == dns.TypeNS {
			apexNS = true
		}
		z.records = append(z.records, copyRR)
		owners[owner] = append(owners[owner], copyRR)
	}
	if z.soa == nil || !apexNS {
		return nil, fmt.Errorf("rpz: complete policy zone requires apex SOA and NS")
	}
	// Transport consumes an initial SOA for IXFR; presentation order otherwise
	// remains unchanged and all records are privately retained.
	for i, rr := range z.records {
		if rr == z.soa {
			copy(z.records[1:i+1], z.records[:i])
			z.records[0] = z.soa
			break
		}
	}
	z.stats.Records = len(z.records)
	keys := make([]string, 0, len(owners))
	for owner := range owners {
		keys = append(keys, owner)
	}
	slices.Sort(keys)
	for _, owner := range keys {
		data, err := policyData(owners[owner], owner == origin)
		if err != nil {
			return nil, fmt.Errorf("rpz: owner %q: %w", owner, err)
		}
		z.stats.Ignored += len(owners[owner]) - len(data)
		if len(data) == 0 {
			continue
		}
		if _, ok := excluded[owner]; ok {
			excluded[owner] = true
			z.stats.Excluded++
			continue
		}
		relative := strings.TrimSuffix(owner, origin)
		r := &rule{entry: Entry{Owner: owner}}
		reason, unsupported := decodeTrigger(r, strings.TrimSuffix(relative, "."))
		if reason == "" && r.entry.Trigger == TriggerQName {
			z.addNodes(r.entry.Value)
		}
		if reason == "" {
			reason, unsupported = decodeAction(r, data)
		}
		if reason != "" {
			z.diagnose(owner, reason, unsupported)
			continue
		}
		if err := z.install(r); err != nil {
			return nil, err
		}
		z.rules = append(z.rules, r)
		z.stats.Rules++
	}
	for _, seen := range excluded {
		if !seen {
			z.stats.StaleExcludes++
		}
	}
	return z, nil
}

func illegalType(typ uint16) bool {
	switch typ {
	case 0, dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY, dns.TypeAXFR, dns.TypeIXFR, dns.TypeANY, dns.TypeMAILA, dns.TypeMAILB:
		return true
	}
	return false
}

func dnssecType(typ uint16) bool {
	switch typ {
	case dns.TypeSIG, dns.TypeKEY, dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3,
		dns.TypeNSEC3PARAM, dns.TypeDS, dns.TypeDNSKEY, dns.TypeCDS, dns.TypeCDNSKEY:
		return true
	}
	return false
}

func policyData(records []dns.RR, apex bool) ([]dns.RR, error) {
	var data []dns.RR
	var cname *dns.CNAME
	ttls := map[uint16]uint32{}
	seen := map[string]bool{}
	for _, rr := range records {
		h := rr.Header()
		if dnssecType(h.Rrtype) || apex && (h.Rrtype == dns.TypeSOA || h.Rrtype == dns.TypeNS) {
			continue
		}
		if prev, ok := ttls[h.Rrtype]; ok && prev != h.Ttl {
			return nil, fmt.Errorf("inconsistent RRset TTL")
		}
		ttls[h.Rrtype] = h.Ttl
		if seen[rr.String()] {
			continue
		}
		seen[rr.String()] = true
		if c, ok := rr.(*dns.CNAME); ok {
			if cname != nil {
				if dns.CanonicalName(cname.Target) == dns.CanonicalName(c.Target) {
					continue
				}
				return nil, fmt.Errorf("multiple CNAME targets")
			}
			cname = c
		}
		data = append(data, rr)
	}
	if cname != nil && len(data) != 1 {
		return nil, fmt.Errorf("CNAME coexists with ordinary policy data")
	}
	return data, nil
}

func decodeTrigger(r *rule, relative string) (string, bool) {
	if relative == "" {
		return "apex-policy", true
	}
	labels := dns.SplitDomainName(relative)
	last := labels[len(labels)-1]
	switch last {
	case "rpz-nsdname", "rpz-nsip":
		return "resolver-ns-provenance", true
	case "rpz-client-ip", "rpz-ip":
		prefix, err := addressPrefix(labels[:len(labels)-1])
		if err != nil {
			return "invalid-address-trigger", false
		}
		r.prefix, r.entry.Value = prefix, prefix.String()
		r.entry.Trigger = TriggerResponseIP
		if last == "rpz-client-ip" {
			r.entry.Trigger = TriggerClientIP
		}
	default:
		if strings.HasPrefix(last, "rpz-") {
			return "unknown-reserved-trigger", true
		}
		for i, label := range labels {
			if strings.Contains(label, "*") && (i != 0 || label != "*") {
				return "nonterminal-wildcard", true
			}
		}
		r.entry.Trigger, r.entry.Value = TriggerQName, dns.Fqdn(relative)
	}
	return "", false
}

func decodeAction(r *rule, records []dns.RR) (string, bool) {
	r.entry.TTL = records[0].Header().Ttl
	for _, rr := range records {
		r.entry.TTL = min(r.entry.TTL, rr.Header().Ttl)
		if rr.Header().Rrtype == dns.TypeDNAME {
			return "dname-action", true
		}
	}
	r.entry.Action = ActionLocalData
	if cname, ok := records[0].(*dns.CNAME); ok {
		target, err := canonicalName(cname.Target)
		if err != nil {
			return "invalid-cname-action", false
		}
		switch target {
		case ".":
			r.entry.Action = ActionNXDOMAIN
		case "*.":
			r.entry.Action = ActionNODATA
		case "rpz-passthru.":
			r.entry.Action = ActionPassthru
		case "rpz-drop.":
			r.entry.Action = ActionDrop
		case "rpz-tcp-only.":
			r.entry.Action = ActionTCPOnly
		default:
			if strings.HasPrefix(target, "rpz-") && dns.CountLabel(target) == 1 {
				return "unknown-reserved-action", true
			}
			labels := dns.SplitDomainName(target)
			for i, label := range labels {
				if strings.Contains(label, "*") && (i != 0 || label != "*") {
					return "nonterminal-cname-wildcard", true
				}
			}
		}
	}
	if r.entry.Action == ActionLocalData {
		r.records = records
	}
	return "", false
}

func (z *Zone) diagnose(owner, reason string, unsupported bool) {
	if unsupported {
		z.stats.Unsupported++
	} else {
		z.stats.Invalid++
	}
	if len(z.diagnostics) < MaxDiagnostics {
		z.diagnostics = append(z.diagnostics, Diagnostic{Owner: owner, Reason: reason, Unsupported: unsupported})
	}
}

func (z *Zone) addNodes(name string) {
	for {
		z.nodes[name] = true
		if name == "." {
			return
		}
		name = parentName(name)
	}
}

func (z *Zone) install(r *rule) error {
	switch r.entry.Trigger {
	case TriggerQName:
		z.qnames[r.entry.Value] = r
	case TriggerClientIP:
		if z.clients[r.prefix] != nil {
			return fmt.Errorf("rpz: duplicate normalized client-IP trigger")
		}
		z.clients[r.prefix] = r
	case TriggerResponseIP:
		if z.addresses[r.prefix] != nil {
			return fmt.Errorf("rpz: duplicate normalized response-IP trigger")
		}
		z.addresses[r.prefix] = r
	}
	return nil
}
