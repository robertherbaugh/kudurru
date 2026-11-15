package secondary

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// IXFR returns a complete replacement for base, which must contain one initial
// SOA and no closing duplicate. It never mutates base or returns partial data.
// Empty base requests AXFR. Primaries may return AXFR instead of deltas, or an
// authenticated NOTIMP response to trigger AXFR on a fresh connection. Both
// attempts share one deadline and cumulative wire/expanded byte budgets.
func (c *TransferClient) IXFR(ctx context.Context, dz *v1alpha1.DNSZone, primary string, base []dns.RR) ([]dns.RR, error) {
	if len(base) == 0 {
		return c.AXFR(ctx, dz, primary)
	}
	if dz == nil || dz.Spec.Secondary == nil {
		return nil, fmt.Errorf("secondary: missing secondary configuration")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxRecords, maxBytes := c.MaxRecords, c.MaxBytes
	if maxRecords <= 0 {
		maxRecords = 1_000_000
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	working, err := newDeltaZone(ctx, dns.CanonicalName(dz.Spec.ZoneName), base, maxRecords, maxBytes)
	if err != nil {
		return nil, err
	}
	s, err := c.openQuery(ctx, dz, primary, dns.TypeIXFR, working.soa)
	if err != nil {
		return nil, err
	}
	defer s.close()
	stream := ixfrStream{zone: working}
	for {
		m, wire, err := s.read()
		if err != nil {
			return nil, fmt.Errorf("secondary: incomplete IXFR: %w", err)
		}
		first := stream.current == nil
		if err := s.authenticate(m, wire, first); err != nil {
			return nil, err
		}
		if err := s.validate(m, first); err != nil {
			var rcode *transferRcodeError
			if first && errors.As(err, &rcode) && rcode.code == dns.RcodeNotImplemented && len(m.Answer) == 0 && len(m.Ns) == 0 {
				// Close the IXFR socket before dialing again, with no TLS downgrade.
				// Authentication and syntax were checked before this fallback.
				wireBytes, decodeBytes := s.wireBytes, s.decodeBytes
				s.close()
				rrs, err := c.axfr(ctx, dz, primary, wireBytes, decodeBytes)
				if err == nil {
					serial := rrs[0].(*dns.SOA).Serial
					if serial != working.soa.Serial && !serialAdvances(working.soa.Serial, serial) {
						return nil, fmt.Errorf("secondary: fallback AXFR serial is older or ambiguous")
					}
				}
				return rrs, err
			}
			return nil, err
		}
		if len(m.Answer) == 0 || len(m.Ns) != 0 {
			return nil, fmt.Errorf("secondary: IXFR requires answers and an empty authority section")
		}
		for i, rr := range m.Answer {
			if err := s.ctx.Err(); err != nil {
				return nil, err
			}
			if err := s.validateRR(rr); err != nil {
				return nil, err
			}
			done, err := stream.consume(rr)
			if err != nil {
				return nil, err
			}
			if first && i == 0 && stream.current.Serial == working.soa.Serial {
				// RFC 1995: a same-serial reply is exactly one current SOA.
				if len(m.Answer) != 1 || !sameTransferSOA(stream.current, working.soa) {
					return nil, fmt.Errorf("secondary: invalid same-serial IXFR confirmation")
				}
				done = true
			}
			if done {
				if i != len(m.Answer)-1 {
					return nil, fmt.Errorf("secondary: records after closing IXFR SOA")
				}
				if s.key != "" && m.IsTsig() == nil {
					return nil, fmt.Errorf("secondary: closing IXFR message: %w", dns.ErrNoSig)
				}
				return working.records(s.ctx)
			}
		}
	}
}

type ixfrPhase uint8

const (
	ixfrInitial ixfrPhase = iota
	ixfrDetect
	ixfrRemove
	ixfrAdd
	ixfrFull
)

type ixfrStream struct {
	zone    *deltaZone
	current *dns.SOA
	phase   ixfrPhase
	edits   int
	steps   int
}

func serialAdvances(from, to uint32) bool { return to != from && to-from < 1<<31 }

func (s *ixfrStream) consume(rr dns.RR) (bool, error) {
	soa, isSOA := rr.(*dns.SOA)
	if isSOA && dns.CanonicalName(soa.Hdr.Name) != s.zone.origin {
		return false, fmt.Errorf("secondary: IXFR contains a non-apex SOA")
	}
	switch s.phase {
	case ixfrInitial:
		if !isSOA {
			return false, fmt.Errorf("secondary: IXFR must start with the zone's SOA")
		}
		if soa.Serial != s.zone.soa.Serial && !serialAdvances(s.zone.soa.Serial, soa.Serial) {
			return false, fmt.Errorf("secondary: IXFR serial is older or ambiguous")
		}
		s.current, s.phase = soa, ixfrDetect
		return false, nil
	case ixfrDetect:
		if !isSOA || sameTransferSOA(soa, s.current) {
			// RFC 1995 AXFR fallback: second record is data or closing SOA.
			s.zone.entries = make(map[string]deltaRecord)
			s.zone.bytes = int64(dns.Len(s.current))
			s.zone.soa, s.phase = s.current, ixfrFull
		} else {
			if !sameTransferSOA(soa, s.zone.soa) {
				return false, fmt.Errorf("secondary: IXFR starts from a different base SOA")
			}
			s.phase = ixfrRemove
			return false, nil
		}
	case ixfrRemove:
		if isSOA {
			if !serialAdvances(s.zone.soa.Serial, soa.Serial) || (soa.Serial != s.current.Serial && !serialAdvances(soa.Serial, s.current.Serial)) {
				return false, fmt.Errorf("secondary: invalid IXFR delta serial transition")
			}
			s.steps++
			if s.steps > s.zone.maxRecords {
				return false, fmt.Errorf("secondary: IXFR exceeds delta step limit")
			}
			s.zone.bytes += int64(dns.Len(soa) - dns.Len(s.zone.soa))
			if s.zone.bytes > s.zone.maxBytes {
				return false, fmt.Errorf("secondary: IXFR exceeds resulting byte limit")
			}
			s.zone.soa, s.phase = soa, ixfrAdd
			return false, nil
		}
	case ixfrAdd:
		if isSOA {
			if !sameTransferSOA(soa, s.zone.soa) {
				return false, fmt.Errorf("secondary: mismatched IXFR delta SOA")
			}
			if soa.Serial == s.current.Serial {
				if !sameTransferSOA(soa, s.current) {
					return false, fmt.Errorf("secondary: mismatched closing IXFR SOA")
				}
				s.zone.soa = s.current
				return true, nil
			}
			s.phase = ixfrRemove
			return false, nil
		}
	}
	if s.phase == ixfrFull {
		if isSOA {
			if !sameTransferSOA(soa, s.current) {
				return false, fmt.Errorf("secondary: mismatched closing fallback AXFR SOA")
			}
			return true, nil
		}
		return false, s.zone.add(rr)
	}
	s.edits++
	if s.edits > s.zone.maxRecords {
		return false, fmt.Errorf("secondary: IXFR exceeds delta record limit")
	}
	if s.phase == ixfrRemove {
		return false, s.zone.remove(rr)
	}
	return false, s.zone.add(rr)
}

type deltaRecord struct {
	rr    dns.RR
	order int
}

type deltaZone struct {
	origin     string
	soa        *dns.SOA
	entries    map[string]deltaRecord
	next       int
	bytes      int64
	maxRecords int
	maxBytes   int64
}

func newDeltaZone(ctx context.Context, origin string, base []dns.RR, maxRecords int, maxBytes int64) (*deltaZone, error) {
	z := &deltaZone{origin: origin, entries: make(map[string]deltaRecord), maxRecords: maxRecords, maxBytes: maxBytes}
	check := &transferSession{origin: origin}
	for i, rr := range base {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rr == nil || reflect.ValueOf(rr).IsNil() {
			return nil, fmt.Errorf("secondary: nil base record")
		}
		if err := check.validateRR(rr); err != nil {
			return nil, err
		}
		soa, isSOA := rr.(*dns.SOA)
		if i == 0 {
			if !isSOA || !check.apexSOA(soa) {
				return nil, fmt.Errorf("secondary: IXFR base must start with an apex SOA")
			}
			z.soa, z.bytes = soa, int64(dns.Len(soa))
			if z.bytes > maxBytes {
				return nil, fmt.Errorf("secondary: IXFR base exceeds byte limit")
			}
		} else if isSOA {
			return nil, fmt.Errorf("secondary: IXFR base contains multiple SOAs")
		} else if err := z.add(rr); err != nil {
			return nil, err
		}
	}
	return z, nil
}

func (z *deltaZone) add(rr dns.RR) error {
	if len(z.entries)+1 >= z.maxRecords {
		return fmt.Errorf("secondary: IXFR exceeds resulting record limit")
	}
	n := int64(dns.Len(rr))
	if n > z.maxBytes-z.bytes {
		return fmt.Errorf("secondary: IXFR exceeds resulting byte limit")
	}
	key, err := deltaKey(rr)
	if err != nil {
		return err
	}
	if _, exists := z.entries[key]; exists {
		return fmt.Errorf("secondary: IXFR adds a duplicate record")
	}
	z.entries[key] = deltaRecord{rr: rr, order: z.next}
	z.next++
	z.bytes += n
	return nil
}

func (z *deltaZone) remove(rr dns.RR) error {
	key, err := deltaKey(rr)
	if err != nil {
		return err
	}
	old, exists := z.entries[key]
	if !exists || old.rr.Header().Ttl != rr.Header().Ttl {
		return fmt.Errorf("secondary: IXFR deletes a missing or mismatched record")
	}
	delete(z.entries, key)
	z.bytes -= int64(dns.Len(old.rr))
	return nil
}

func (z *deltaZone) records(ctx context.Context) ([]dns.RR, error) {
	entries := make([]deltaRecord, 0, len(z.entries))
	for _, entry := range z.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b deltaRecord) int { return a.order - b.order })
	rrs := make([]dns.RR, 1, len(entries)+1)
	rrs[0] = dns.Copy(z.soa)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rrs = append(rrs, dns.Copy(entry.rr))
	}
	return rrs, ctx.Err()
}

// deltaKey identifies a DNS RR independent of TTL and presentation-case for
// domain-name fields. Opaque and text RDATA remains byte/case sensitive. Pack a
// copy so normalization never changes the raw base or transferred records.
func deltaKey(rr dns.RR) (string, error) {
	copyRR := dns.Copy(rr)
	copyRR.Header().Name = dns.CanonicalName(copyRR.Header().Name)
	copyRR.Header().Ttl = 0
	canonicalDeltaNames(reflect.ValueOf(copyRR).Elem())
	wire := make([]byte, dns.Len(copyRR)+16)
	n, err := dns.PackRR(copyRR, wire, 0, nil, false)
	if err != nil {
		return "", fmt.Errorf("secondary: invalid IXFR record: %w", err)
	}
	return string(wire[:n]), nil
}

func canonicalDeltaNames(v reflect.Value) {
	for i := range v.NumField() {
		field := v.Field(i)
		tag := v.Type().Field(i).Tag.Get("dns")
		if field.Kind() == reflect.Struct {
			canonicalDeltaNames(field) // Includes embedded HTTPS/SVCB headers.
		} else if tag == "domain-name" || tag == "cdomain-name" || tag == "ipsechost" || tag == "amtrelayhost" {
			switch field.Kind() {
			case reflect.String:
				field.SetString(dns.CanonicalName(field.String()))
			case reflect.Slice:
				for j := range field.Len() {
					field.Index(j).SetString(dns.CanonicalName(field.Index(j).String()))
				}
			}
		}
	}
}
