package zonestate

import (
	"fmt"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// ReplaceStatic replaces the complete desired projection, retaining dynamic
// records and receipts. Takeover must be explicitly authorized by the caller.
// Static conflicts fail the whole candidate, including inactive reservations.
func ReplaceStatic(base *View, static *Aggregate, takeover bool, now time.Time, l Limits) (*Aggregate, error) {
	l = l.Defaults()
	if static == nil {
		return nil, ErrInvalid
	}
	if err := aggregatePreflight(static, l); err != nil {
		return nil, err
	}
	out := *static
	out.Records = append([]Record(nil), static.Records...)
	if base == nil || base.Head == nil {
		return &out, nil
	}
	if base.Aggregate == nil || base.Aggregate.Zone != static.Zone {
		return nil, ErrInvalid
	}
	protected := append([]dynamicupdate.Reservation(nil), static.Reservations...)
	for _, r := range static.Records {
		if r.Provenance.SourceID == "" {
			return nil, ErrInvalid
		}
		rr, n, err := dns.UnpackRR(r.Wire, 0)
		if err != nil || n != len(r.Wire) {
			return nil, ErrInvalid
		}
		protected = append(protected, dynamicupdate.Reservation{Name: rr.Header().Name, Type: rr.Header().Rrtype, Subtree: rr.Header().Rrtype == dns.TypeDNAME, Source: r.Provenance.SourceID})
	}
	// Conservative root-supplied hosted-child fences survive ordinary desired recompilation.
	for _, r := range base.Aggregate.Reservations {
		if r.Source == "system" {
			out.Reservations = append(out.Reservations, r)
			protected = append(protected, r)
		}
	}
	for _, r := range base.Aggregate.Records {
		if r.Provenance.SourceID != "" {
			continue
		}
		rr, n, err := dns.UnpackRR(r.Wire, 0)
		if err != nil || n != len(r.Wire) {
			return nil, ErrInvalid
		}
		if projectionConflicts(rr, protected) {
			if !takeover {
				return nil, fmt.Errorf("%w: static projection conflicts with dynamic ownership", ErrConflict)
			}
			continue
		}
		out.Records = append(out.Records, r)
	}
	out.Receipts = append([]Receipt(nil), base.Aggregate.Receipts...)
	_, hash, _, err := normalized(&out, l)
	if err != nil {
		return nil, err
	}
	serial := base.Head.Spec.Serial
	if hash != base.Head.Spec.ContentHash {
		serial = dynamicupdate.AutoSerial(serial, now)
	}
	for i, r := range out.Records {
		rr, _, err := dns.UnpackRR(r.Wire, 0)
		if err != nil {
			return nil, err
		}
		if soa, ok := rr.(*dns.SOA); ok {
			soa.Serial = serial
			out.Records[i], err = FromRR(soa, r.Provenance)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	return &out, nil
}

func projectionConflicts(rr dns.RR, reservations []dynamicupdate.Reservation) bool {
	name, err := dynamicupdate.CanonicalName(rr.Header().Name)
	if err != nil {
		return true
	}
	for _, r := range reservations {
		n, err := dynamicupdate.CanonicalName(r.Name)
		if err != nil {
			return true
		}
		if name == n && (r.Type == 0 || r.Type == rr.Header().Rrtype || r.Type == dns.TypeCNAME || rr.Header().Rrtype == dns.TypeCNAME) {
			return true
		}
		if r.Subtree && dns.IsSubDomain(n, name) {
			return true
		}
		if rr.Header().Rrtype == dns.TypeDNAME && dns.IsSubDomain(name, n) {
			return true
		}
	}
	return false
}

// DynamicOwnership indexes the records a dynamic update added to a
// committed view, so a compiler can check each static endpoint against
// them before it takes the endpoint. Conflicts applies the rule
// ReplaceStatic applies to the static records and reservations of a
// whole candidate, one static record at a time: a primary compiler
// leaves out only the endpoints that collide, so one endpoint written at
// a name a dynamic update owns never withdraws the rest of its record set
// or keeps the rest of the zone from its commit (D373). A dynamic record
// whose name is not canonical is not indexed; ReplaceStatic still
// refuses the candidate for it.
//
// The index is by name, so Conflicts costs a few map lookups per label of
// the static name and never a scan of the dynamic records: a zone at the
// 100,000-source limit with many dynamic DNAMEs compiles in time linear in
// its static records.
type DynamicOwnership struct {
	// byName holds the dynamic records at each canonical owner name.
	byName map[string][]dns.RR
	// covered holds every name at or above a dynamic record's owner name
	// (the record's name and each of its ancestors): a static DNAME at a
	// covered name has a dynamic record at or below it.
	covered map[string]bool
	// dnames holds the owner names of the dynamic DNAMEs: a static record
	// at or below one of them conflicts with it.
	dnames map[string]bool
}

// NewDynamicOwnership indexes the dynamic records of base. A view without
// a committed head has none: ReplaceStatic checks nothing against it.
func NewDynamicOwnership(base *View) (*DynamicOwnership, error) {
	o := &DynamicOwnership{byName: map[string][]dns.RR{}, covered: map[string]bool{}, dnames: map[string]bool{}}
	if base == nil || base.Head == nil || base.Aggregate == nil {
		return o, nil
	}
	for _, r := range base.Aggregate.Records {
		if r.Provenance.SourceID != "" {
			continue
		}
		rr, n, err := dns.UnpackRR(r.Wire, 0)
		if err != nil || n != len(r.Wire) {
			return nil, ErrInvalid
		}
		name, err := dynamicupdate.CanonicalName(rr.Header().Name)
		if err != nil {
			continue
		}
		o.byName[name] = append(o.byName[name], rr)
		if rr.Header().Rrtype == dns.TypeDNAME {
			o.dnames[name] = true
		}
		if !o.covered[name] {
			ancestors(name, func(a string) bool {
				if o.covered[a] {
					return false // its ancestors are covered already
				}
				o.covered[a] = true
				return true
			})
		}
	}
	return o, nil
}

// Empty reports whether no dynamic record is indexed.
func (o *DynamicOwnership) Empty() bool { return o == nil || len(o.byName) == 0 }

// ancestors calls fn with the canonical name and then each of its
// ancestors up to the root, until fn returns false.
func ancestors(name string, fn func(string) bool) {
	for _, i := range dns.Split(name) {
		if !fn(name[i:]) {
			return
		}
	}
	fn(".")
}

// Conflicts reports whether the static record rr (served, or reserved for
// an endpoint that is not served now) conflicts with a dynamic record.
func (o *DynamicOwnership) Conflicts(rr dns.RR) bool {
	if o.Empty() {
		return false
	}
	name, err := dynamicupdate.CanonicalName(rr.Header().Name)
	if err != nil {
		// ReplaceStatic refuses a name that is not canonical.
		return false
	}
	r := []dynamicupdate.Reservation{{Name: name, Type: rr.Header().Rrtype, Subtree: rr.Header().Rrtype == dns.TypeDNAME}}
	for _, d := range o.byName[name] {
		if projectionConflicts(d, r) {
			return true
		}
	}
	// A static DNAME conflicts with every dynamic record at or below it.
	if r[0].Subtree && o.covered[name] {
		return true
	}
	// A dynamic DNAME conflicts with every static record at or below it.
	conflict := false
	ancestors(name, func(a string) bool {
		conflict = o.dnames[a]
		return !conflict
	})
	return conflict
}
