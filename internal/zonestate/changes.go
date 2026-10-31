package zonestate

import (
	"bytes"
	"fmt"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// ApplyResult attaches provenance to a pure UPDATE plan while independently
// defending static ownership. Generated SOA may change ONLY its serial.
func ApplyResult(a *Aggregate, r *dynamicupdate.Result, p dynamicupdate.Principal) (*Aggregate, error) {
	if a == nil || r == nil || r.AuthEpoch != a.Policy.AuthEpoch || r.CredentialVersion != p.Version {
		return nil, ErrInvalid
	}
	old := map[string]Record{}
	static := map[string]bool{}
	for _, record := range a.Records {
		rr, n, err := dns.UnpackRR(record.Wire, 0)
		if err != nil || n != len(record.Wire) {
			return nil, ErrInvalid
		}
		key, err := dynamicupdate.Identity(rr)
		if err != nil {
			return nil, err
		}
		old[key] = record
		if record.Provenance.SourceID != "" {
			static[key] = true
		}
	}
	out := *a
	out.Records = make([]Record, 0, len(r.Records))
	for _, rr := range r.Records {
		key, err := dynamicupdate.Identity(rr)
		if err != nil {
			return nil, err
		}
		previous, exists := old[key]
		provenance := Provenance{Key: p.Key, CredentialVersion: p.Version}
		if exists {
			provenance = previous.Provenance
			delete(static, key)
		}
		if rr.Header().Rrtype == dns.TypeSOA {
			for oldKey, record := range old {
				prior, _, err := dns.UnpackRR(record.Wire, 0)
				if err != nil {
					return nil, err
				}
				soa, ok := prior.(*dns.SOA)
				if !ok {
					continue
				}
				next, ok := rr.(*dns.SOA)
				if !ok {
					return nil, ErrInvalid
				}
				soa.Serial = next.Serial
				priorWire, err := dynamicupdate.Wire(soa)
				if err != nil {
					return nil, err
				}
				nextWire, err := dynamicupdate.Wire(rr)
				if err != nil || !bytes.Equal(priorWire, nextWire) {
					return nil, fmt.Errorf("%w: generated SOA fields protected", ErrInvalid)
				}
				provenance = record.Provenance
				delete(static, oldKey)
				break
			}
		}
		record, err := FromRR(rr, provenance)
		if err != nil {
			return nil, err
		}
		if exists && previous.Provenance.SourceID != "" && !bytes.Equal(previous.Wire, record.Wire) {
			return nil, fmt.Errorf("%w: static TTL/data protected", ErrInvalid)
		}
		out.Records = append(out.Records, record)
	}
	if len(static) > 0 {
		return nil, fmt.Errorf("%w: static record deletion", ErrInvalid)
	}
	return &out, nil
}

// Projection is compiled from an ACTUAL freshly read desired source after
// reading the head. The root controller retains source precedence/selection
// and unreadable-file last-good rules. Removed=true is an intentional prune.
type Projection struct {
	Source       Source
	Records      []dns.RR
	Reservations []dynamicupdate.Reservation
	Removed      bool
	Takeover     bool
}

// ReplaceSource modifies one fragment without losing other writers' data.
// Dynamic conflicts refuse the whole projection unless takeover was explicitly
// authorized/audited by the caller. Disabled/view-scoped reservations count.
func ReplaceSource(a *Aggregate, p Projection, now time.Time, l Limits) (*Aggregate, error) {
	l = l.Defaults()
	if a == nil || p.Source.ID == "" || p.Source.ID == "system" || len(p.Records) > l.MaxRecords || len(p.Reservations) > l.MaxRecords {
		return nil, ErrInvalid
	}
	out := *a
	out.Sources = nil
	out.Reservations = nil
	out.Records = nil
	for _, s := range a.Sources {
		if s.ID != p.Source.ID {
			out.Sources = append(out.Sources, s)
		}
	}
	for _, r := range a.Reservations {
		if r.Source != p.Source.ID {
			out.Reservations = append(out.Reservations, r)
		}
	}
	var reserved []dynamicupdate.Reservation
	if !p.Removed {
		out.Sources = append(out.Sources, p.Source)
		for _, r := range p.Reservations {
			r.Source = p.Source.ID
			reserved = append(reserved, r)
		}
		for _, rr := range p.Records {
			if rr == nil {
				return nil, ErrInvalid
			}
			h := rr.Header()
			reserved = append(reserved, dynamicupdate.Reservation{Name: h.Name, Type: h.Rrtype, Subtree: h.Rrtype == dns.TypeDNAME, Source: p.Source.ID})
		}
	}
	conflicts := func(rr dns.RR) bool {
		name, err := dynamicupdate.CanonicalName(rr.Header().Name)
		if err != nil {
			return true
		}
		for _, r := range reserved {
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
	for _, r := range a.Records {
		if r.Provenance.SourceID == p.Source.ID {
			continue
		}
		if r.Provenance.SourceID == "" {
			rr, n, err := dns.UnpackRR(r.Wire, 0)
			if err != nil || n != len(r.Wire) {
				return nil, ErrInvalid
			}
			if conflicts(rr) {
				if !p.Takeover {
					return nil, fmt.Errorf("%w: desired source conflicts with dynamic ownership", ErrConflict)
				}
				continue
			}
		}
		out.Records = append(out.Records, r)
	}
	// Keep only caller-supplied inactive reservations; visible source records
	// automatically reserve their identities in View.UpdateState.
	if !p.Removed {
		for _, r := range p.Reservations {
			r.Source = p.Source.ID
			out.Reservations = append(out.Reservations, r)
		}
		for _, rr := range p.Records {
			r, err := FromRR(rr, Provenance{SourceID: p.Source.ID})
			if err != nil {
				return nil, err
			}
			out.Records = append(out.Records, r)
		}
	}
	_, before, oldSerial, err := normalized(a, l)
	if err != nil {
		return nil, err
	}
	_, after, _, err := normalized(&out, l)
	if err != nil {
		return nil, err
	}
	if before != after {
		for i, r := range out.Records {
			rr, _, err := dns.UnpackRR(r.Wire, 0)
			if err != nil {
				return nil, err
			}
			if soa, ok := rr.(*dns.SOA); ok {
				soa.Serial = dynamicupdate.AutoSerial(oldSerial, now)
				updated, err := FromRR(soa, r.Provenance)
				if err != nil {
					return nil, err
				}
				out.Records[i] = updated
				break
			}
		}
	}
	return &out, nil
}
