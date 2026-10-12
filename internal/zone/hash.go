package zone

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"math"
	"time"

	"github.com/miekg/dns"
)

// HashPrefix prefixes every content hash so the algorithm can change
// without ambiguity.
const HashPrefix = "sha256:"

// contentHash is sha256 over every served RR in canonical order (owner
// name in RFC 4034 §6.1 order, then type, then canonical rdata). Each RR
// contributes its canonical owner key, type, class, TTL and canonical
// rdata, length-prefixed so the encoding is unambiguous. The SOA serial is
// zeroed: the serial is derived from the hash (D12), not part of it.
func contentHash(z *Zone) string {
	h := sha256.New()
	var buf [10]byte
	z.RRsets(func(s *RRset) bool {
		k, _ := canonicalKey(s.Name)
		for _, rr := range s.RRs {
			if soa, ok := rr.(*dns.SOA); ok {
				c := *soa
				c.Serial = 0
				rr = &c
			}
			writeRR(h, &buf, k, rr)
		}
		return true
	})
	return HashPrefix + hex.EncodeToString(h.Sum(nil))
}

func writeRR(h hash.Hash, buf *[10]byte, key string, rr dns.RR) {
	rd := canonicalRdata(rr)
	hdr := rr.Header()
	// Keys are at most 255 octets plus escapes and rdata at most 65535
	// octets (it packed), so the uint16 conversions are exact.
	binary.BigEndian.PutUint16(buf[0:], uint16(len(key))) //nolint:gosec // G115: bounded, see above
	h.Write(buf[:2])
	h.Write([]byte(key))
	binary.BigEndian.PutUint16(buf[0:], hdr.Rrtype)
	binary.BigEndian.PutUint16(buf[2:], hdr.Class)
	binary.BigEndian.PutUint32(buf[4:], hdr.Ttl)
	binary.BigEndian.PutUint16(buf[8:], uint16(len(rd))) //nolint:gosec // G115: bounded, see above
	h.Write(buf[:10])
	h.Write(rd)
}

// hashForward hashes what a forward zone serves: its upstream group and
// query policy are its whole content.
func hashForward(z *Zone) string {
	h := sha256.New()
	h.Write([]byte("forward\x00" + z.name + "\x00" + z.upstream))
	return HashPrefix + hex.EncodeToString(h.Sum(nil))
}

// NextSerial implements the serial rule of decision D12:
// max(prev+1, unix_now). prev+1 wraps to 0 after 2^32-1 as RFC 1982
// serial arithmetic does; the result is then unix_now, which is still
// "greater" than prev in RFC 1982 terms. Times before 1970 count as 0 and
// times after 2106 wrap modulo 2^32.
func NextSerial(prev uint32, now time.Time) uint32 {
	next := prev + 1 // wraps
	u := now.Unix()
	if u < 0 {
		u = 0
	}
	n := uint32(uint64(u) % (math.MaxUint32 + 1))
	return max(next, n)
}

// StampSerial is what the leader runs when it observes a zone build: when
// the content hash differs from the stamped one (or nothing was stamped
// yet) it returns the next serial and true, otherwise the previous serial
// and false. The controller writes the result to status.serial and
// status.contentHash in one UpdateStatus.
func StampSerial(prevSerial uint32, prevHash, newHash string, now time.Time) (uint32, bool) {
	if prevHash == newHash && prevSerial != 0 {
		return prevSerial, false
	}
	return NextSerial(prevSerial, now), true
}
