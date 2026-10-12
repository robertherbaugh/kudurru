package zone

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/miekg/dns"
)

// Delta is one serial transition of a zone in IXFR form (RFC 1995): the
// RRs removed from the FromSerial version and the RRs added to reach
// ToSerial. SOA records are not listed; an IXFR writer frames each delta
// with the old and new SOA.
type Delta struct {
	Zone       string
	FromSerial uint32
	ToSerial   uint32
	Removed    []dns.RR
	Added      []dns.RR
}

// Empty reports whether the delta changes no RRs.
func (d *Delta) Empty() bool { return len(d.Removed) == 0 && len(d.Added) == 0 }

// ErrJournalGap is returned by Journal.Since when the journal no longer
// holds the requested serial; the caller falls back to AXFR.
var ErrJournalGap = errors.New("zone: journal does not reach back to the requested serial")

// Journal is the IXFR seam (v0.2). The zone cache appends the Diff of
// every rebuild that changed the serial; the transfer stage replays deltas
// newer than a secondary's serial. Implementations keep the journal in
// local.db and bound it by size and age. No implementation ships in v0.1.
type Journal interface {
	// Append records d. Deltas for one zone must be appended in serial
	// order with d.FromSerial equal to the previous d.ToSerial.
	Append(ctx context.Context, d Delta) error
	// Since returns the deltas that take zone from serial to the newest
	// version, oldest first, or ErrJournalGap.
	Since(ctx context.Context, zone string, serial uint32) ([]Delta, error)
	// Truncate drops deltas that end at or before serial.
	Truncate(ctx context.Context, zone string, serial uint32) error
}

// Diff returns the delta that turns prev into next (both built from the
// same zone name). The SOA is excluded; FromSerial and ToSerial are the two
// zones' serials. A nil prev means the empty zone.
func Diff(prev, next *Zone) Delta {
	d := Delta{Zone: next.name, ToSerial: next.serial}
	type entry struct {
		rr   dns.RR
		seen bool
	}
	before := map[string]*entry{}
	if prev != nil {
		d.FromSerial = prev.serial
		prev.RRsets(func(s *RRset) bool {
			if s.Type == dns.TypeSOA {
				return true
			}
			for _, rr := range s.RRs {
				before[diffKey(s, rr)] = &entry{rr: rr}
			}
			return true
		})
	}
	next.RRsets(func(s *RRset) bool {
		if s.Type == dns.TypeSOA {
			return true
		}
		for _, rr := range s.RRs {
			if e, ok := before[diffKey(s, rr)]; ok {
				e.seen = true
				continue
			}
			d.Added = append(d.Added, rr)
		}
		return true
	})
	if prev != nil {
		prev.RRsets(func(s *RRset) bool {
			if s.Type == dns.TypeSOA {
				return true
			}
			for _, rr := range s.RRs {
				if !before[diffKey(s, rr)].seen {
					d.Removed = append(d.Removed, rr)
				}
			}
			return true
		})
	}
	return d
}

func diffKey(s *RRset, rr dns.RR) string {
	k, _ := canonicalKey(s.Name)
	var b [6]byte
	binary.BigEndian.PutUint16(b[0:], s.Type)
	binary.BigEndian.PutUint32(b[2:], s.TTL)
	return k + "\xff" + string(b[:]) + string(canonicalRdata(rr))
}
