package zone

import (
	"iter"

	"github.com/miekg/dns"
)

// MaxTransferMessage is the largest DNS message a TCP transfer stream may
// carry (RFC 1035 §4.2.2: a two-octet length prefix).
const MaxTransferMessage = dns.MaxMsgSize

// TransferOverhead is reserved in every transfer message for the header,
// the echoed question, an OPT record and a TSIG record (key and algorithm
// names plus a 64-octet HMAC-SHA512 MAC), so a chunk of DefaultChunkBytes
// never overflows MaxTransferMessage once those are added.
const TransferOverhead = 1024

// DefaultChunkBytes is the budget of resource records per transfer
// message: MaxTransferMessage minus TransferOverhead. Records are counted
// uncompressed, so the packed (compressed) message is smaller still.
const DefaultChunkBytes = MaxTransferMessage - TransferOverhead

// AXFR returns the zone as an AXFR stream (RFC 5936 §2.2): every record in
// canonical order (owner name, then type, then canonical rdata) with the
// SOA first and repeated last, split into chunks whose uncompressed wire
// size stays within maxBytes (DefaultChunkBytes when maxBytes <= 0). A
// single record larger than the budget gets a chunk of its own. The
// records are the zone's shared, read-only values. Forward zones yield
// nothing. The zone is immutable, so the sequence can be iterated lazily
// and repeatedly.
func (z *Zone) AXFR(maxBytes int) iter.Seq[[]dns.RR] {
	if maxBytes <= 0 {
		maxBytes = DefaultChunkBytes
	}
	return func(yield func([]dns.RR) bool) {
		if z.soa == nil {
			return
		}
		var chunk []dns.RR
		size := 0
		add := func(rr dns.RR) bool {
			n := dns.Len(rr)
			if len(chunk) > 0 && size+n > maxBytes {
				if !yield(chunk) {
					return false
				}
				chunk, size = nil, 0
			}
			chunk = append(chunk, rr)
			size += n
			return true
		}
		ok := true
		z.RRsets(func(s *RRset) bool {
			for _, rr := range s.RRs {
				if ok = add(rr); !ok {
					return false
				}
			}
			return true
		})
		if !ok || !add(z.soa) {
			return
		}
		yield(chunk)
	}
}

// SerialNewer reports whether serial a is greater than b in RFC 1982
// serial number arithmetic on 32 bits. Neither is greater when they are
// exactly 2^31 apart.
func SerialNewer(a, b uint32) bool {
	d := a - b
	return d != 0 && d < 1<<31
}

// UpToDate reports whether a secondary holding clientSerial needs nothing
// from the zone: its serial equals the zone's or is newer in RFC 1982
// terms (RFC 1995 §2: answer an IXFR with the single current SOA).
func (z *Zone) UpToDate(clientSerial uint32) bool {
	return clientSerial == z.serial || SerialNewer(clientSerial, z.serial)
}
