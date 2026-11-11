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
	return conflictingSources(rr, reservations, nil)
}

// conflictingSources reports whether the dynamic record rr conflicts with
// any of reservations, and adds the Source of every reservation it
// conflicts with to into (when into is not nil). A name that is not
// canonical conflicts with everything and is attributed to no source.
func conflictingSources(rr dns.RR, reservations []dynamicupdate.Reservation, into map[string]bool) bool {
	name, err := dynamicupdate.CanonicalName(rr.Header().Name)
	if err != nil {
		return true
	}
	conflict := false
	for _, r := range reservations {
		n, err := dynamicupdate.CanonicalName(r.Name)
		if err != nil {
			return true
		}
		if (name == n && (r.Type == 0 || r.Type == rr.Header().Rrtype || r.Type == dns.TypeCNAME || rr.Header().Rrtype == dns.TypeCNAME)) ||
			(r.Subtree && dns.IsSubDomain(n, name)) ||
			(rr.Header().Rrtype == dns.TypeDNAME && dns.IsSubDomain(name, n)) {
			conflict = true
			if into == nil {
				return true
			}
			into[r.Source] = true
		}
	}
	return conflict
}

// StaticConflicts names the desired sources (the Source of a static record
// or reservation) whose projection conflicts with a dynamic record of base:
// the sources ReplaceStatic would refuse without a takeover. A primary
// compiler leaves those sources out and reports them instead of failing the
// whole candidate, so one record set written at a name a dynamic update
// owns never keeps the rest of the zone from its commit (D373). A
// conflict no source can be named for is not reported here; ReplaceStatic
// still refuses it.
func StaticConflicts(base *View, static *Aggregate) (map[string]bool, error) {
	sources := map[string]bool{}
	if base == nil || base.Head == nil || base.Aggregate == nil || static == nil {
		return sources, nil
	}
	protected := append([]dynamicupdate.Reservation(nil), static.Reservations...)
	for _, r := range static.Records {
		rr, n, err := dns.UnpackRR(r.Wire, 0)
		if err != nil || n != len(r.Wire) {
			return nil, ErrInvalid
		}
		protected = append(protected, dynamicupdate.Reservation{Name: rr.Header().Name, Type: rr.Header().Rrtype, Subtree: rr.Header().Rrtype == dns.TypeDNAME, Source: r.Provenance.SourceID})
	}
	for _, r := range base.Aggregate.Reservations {
		if r.Source == "system" {
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
		conflictingSources(rr, protected, sources)
	}
	return sources, nil
}
