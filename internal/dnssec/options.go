// Package dnssec authenticates forwarded DNS answers against explicit trust
// anchors. It does not sign zones or trust an upstream's AD bit.
package dnssec

import (
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/miekg/dns"
)

// NTA is a temporary operator-authorized exception, never renewed on restart.
type NTA struct {
	Domain    string
	Reason    string
	ExpiresAt time.Time
}

type Limits struct {
	Queries, Hops, RRsets, Signatures, WireBytes, HashWork int
	NSEC3Iterations                                        uint16
}

type Options struct {
	// Nil anchors uses the shipped IANA root DS set. An empty non-nil set is invalid.
	Anchors []dns.RR
	NTAs    []NTA
	Limits  Limits
	Now     func() time.Time
}

// RootAnchors returns private copies of the current IANA DS records.
// Source: https://data.iana.org/root-anchors/root-anchors.xml, retrieved
// 2026-10-14; detached CMS signature verified with IANA's icannbundle.pem.
// Anchors are operator/software managed; fetched keys never become anchors.
func RootAnchors() []dns.RR {
	var out []dns.RR
	for _, s := range []string{
		". 0 IN DS 20326 8 2 E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D",
		". 0 IN DS 38696 8 2 683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16",
	} {
		rr, _ := dns.NewRR(s)
		out = append(out, rr)
	}
	return out
}

type Validator struct {
	anchors map[string][]dns.RR
	ntas    []NTA
	limits  Limits
	now     func() time.Time
	id      [32]byte
}

func New(opts Options) (*Validator, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Anchors == nil {
		opts.Anchors = RootAnchors()
	}
	if len(opts.Anchors) == 0 {
		return nil, fmt.Errorf("dnssec: trust anchors required")
	}
	l := opts.Limits
	if l.Queries == 0 {
		l.Queries = 32
	}
	if l.Hops == 0 {
		l.Hops = 20
	}
	if l.RRsets == 0 {
		l.RRsets = 64
	}
	if l.Signatures == 0 {
		l.Signatures = 16
	}
	if l.WireBytes == 0 {
		l.WireBytes = 1 << 20
	}
	if l.HashWork == 0 {
		l.HashWork = 10000
	}
	if l.NSEC3Iterations == 0 {
		l.NSEC3Iterations = 150
	}
	if l.Queries < 1 || l.Queries > 128 || l.Hops < 1 || l.Hops > 64 || l.RRsets < 1 || l.RRsets > 256 || l.Signatures < 1 || l.Signatures > 64 || l.WireBytes < 1 || l.WireBytes > 8<<20 || l.HashWork < 1 || l.HashWork > 100000 || l.NSEC3Iterations > 150 {
		return nil, fmt.Errorf("dnssec: limits outside supported bounds")
	}
	v := &Validator{anchors: map[string][]dns.RR{}, limits: l, now: opts.Now}
	var identity []string
	for _, rr := range opts.Anchors {
		if nilRR(rr) || rr.Header().Name == "" || rr.Header().Class != dns.ClassINET {
			return nil, fmt.Errorf("dnssec: invalid trust anchor")
		}
		c := dns.Copy(rr)
		name := dns.CanonicalName(c.Header().Name)
		if _, ok := dns.IsDomainName(name); !ok {
			return nil, fmt.Errorf("dnssec: invalid anchor owner")
		}
		c.Header().Name, c.Header().Ttl = name, 0
		switch k := c.(type) {
		case *dns.DS:
			b, err := hex.DecodeString(k.Digest)
			if k.Hdr.Rrtype != dns.TypeDS || err != nil || (k.DigestType != dns.SHA256 && k.DigestType != dns.SHA384) || (k.DigestType == dns.SHA256 && len(b) != 32) || (k.DigestType == dns.SHA384 && len(b) != 48) || !supported(k.Algorithm) {
				return nil, fmt.Errorf("dnssec: unsupported or invalid DS anchor")
			}
		case *dns.DNSKEY:
			if k.Hdr.Rrtype != dns.TypeDNSKEY || k.Protocol != 3 || k.Flags&dns.ZONE == 0 || k.Flags&dns.REVOKE != 0 || !supported(k.Algorithm) || !validPublicKey(k) {
				return nil, fmt.Errorf("dnssec: invalid DNSKEY anchor")
			}
		default:
			return nil, fmt.Errorf("dnssec: anchor must be DS or DNSKEY")
		}
		v.anchors[name] = append(v.anchors[name], c)
		identity = append(identity, c.String())
	}
	seenNTAs := map[string]bool{}
	for _, nta := range opts.NTAs {
		if nta.Domain == "" || strings.IndexFunc(nta.Domain, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("dnssec: NTA domain is required")
		}
		nta.Domain = dns.CanonicalName(nta.Domain)
		if _, ok := dns.IsDomainName(nta.Domain); !ok || strings.TrimSpace(nta.Reason) == "" || len(nta.Reason) > 256 || nta.ExpiresAt.IsZero() || nta.ExpiresAt.After(v.now().Add(7*24*time.Hour)) || seenNTAs[nta.Domain] {
			return nil, fmt.Errorf("dnssec: NTA requires a domain, reason, and expiry within 7 days")
		}
		seenNTAs[nta.Domain] = true
		v.ntas = append(v.ntas, nta)
	}
	sort.Strings(identity)
	sort.Slice(v.ntas, func(i, j int) bool {
		if v.ntas[i].Domain != v.ntas[j].Domain {
			return v.ntas[i].Domain < v.ntas[j].Domain
		}
		return v.ntas[i].ExpiresAt.Before(v.ntas[j].ExpiresAt)
	})
	b, _ := json.Marshal(struct {
		Anchors []string
		NTAs    []NTA
		Limits  Limits
	}{identity, v.ntas, l})
	v.id = sha256.Sum256(b)
	return v, nil
}

func nilRR(rr dns.RR) bool {
	return rr == nil || (reflect.ValueOf(rr).Kind() == reflect.Pointer && reflect.ValueOf(rr).IsNil())
}

func supported(a uint8) bool {
	switch a {
	case dns.RSASHA256, dns.RSASHA512, dns.ECDSAP256SHA256, dns.ECDSAP384SHA384, dns.ED25519:
		return true
	}
	return false
}

func (v *Validator) ContextID() [32]byte { return v.id }

func (v *Validator) anchorBoundary(name string) string {
	best := "."
	for owner := range v.anchors {
		if dns.IsSubDomain(owner, name) && dns.CountLabel(owner) > dns.CountLabel(best) {
			best = owner
		}
	}
	return best
}

// Validate algorithm-specific key material before accepting configuration.
// Bounds on RSA keys also bound expensive verification on the query path.
func validPublicKey(k *dns.DNSKEY) bool {
	if len(k.PublicKey) > 4096 || strings.IndexFunc(k.PublicKey, unicode.IsSpace) >= 0 {
		return false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(k.PublicKey)
	if err != nil {
		return false
	}
	switch k.Algorithm {
	case dns.RSASHA256, dns.RSASHA512:
		if len(b) < 3 {
			return false
		}
		n, off := int(b[0]), 1
		if n == 0 {
			n = int(b[1])<<8 | int(b[2])
			off = 3
			if n < 256 {
				return false
			}
		}
		if n < 1 || n > 4 || off+n >= len(b) || b[off] == 0 || b[off+n] == 0 {
			return false
		}
		e := new(big.Int).SetBytes(b[off : off+n])
		modulus := new(big.Int).SetBytes(b[off+n:])
		return e.Cmp(big.NewInt(3)) >= 0 && e.Bit(0) == 1 && modulus.BitLen() >= 1024 && modulus.BitLen() <= 8192 && modulus.Bit(0) == 1
	case dns.ECDSAP256SHA256, dns.ECDSAP384SHA384:
		var curve elliptic.Curve = elliptic.P256()
		size := 64
		if k.Algorithm == dns.ECDSAP384SHA384 {
			curve = elliptic.P384()
			size = 96
		}
		if len(b) != size {
			return false
		}
		x, y := elliptic.Unmarshal(curve, append([]byte{4}, b...))
		return x != nil && y != nil
	case dns.ED25519:
		if len(b) != 32 {
			return false
		}
		sign := b[31] >> 7
		b[31] &= 127
		for i, j := 0, 31; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
		p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
		y := new(big.Int).SetBytes(b)
		if y.Cmp(p) >= 0 || y.Sign() == 0 || y.Cmp(big.NewInt(1)) == 0 || y.Cmp(new(big.Int).Sub(p, big.NewInt(1))) == 0 {
			return false
		}
		d := new(big.Int).ModInverse(big.NewInt(121666), p)
		d.Mul(d, big.NewInt(-121665)).Mod(d, p)
		y2 := new(big.Int).Mul(y, y)
		y2.Mod(y2, p)
		den := new(big.Int).Mul(d, y2)
		den.Add(den, big.NewInt(1)).Mod(den, p)
		inverse := new(big.Int).ModInverse(den, p)
		if inverse == nil {
			return false
		}
		x2 := new(big.Int).Sub(y2, big.NewInt(1))
		x2.Mul(x2, inverse).Mod(x2, p)
		x := new(big.Int).ModSqrt(x2, p)
		return x != nil && (x.Sign() != 0 || sign == 0)
	}
	return false
}
func (v *Validator) Now() time.Time { return v.now() }

func (v *Validator) exception(name string, now time.Time) (time.Time, bool) {
	var until time.Time
	best := -1
	for _, n := range v.ntas {
		if now.Before(n.ExpiresAt) && dns.IsSubDomain(n.Domain, name) {
			labels := dns.CountLabel(n.Domain)
			if labels > best || (labels == best && (until.IsZero() || n.ExpiresAt.Before(until))) {
				until, best = n.ExpiresAt, labels
			}
		}
	}
	return until, best >= 0
}

// ActiveContextID includes time-dependent exception activation, so an NTA
// expiry cannot reuse a bypass cache entry even before a background tick.
func (v *Validator) ActiveContextID() [32]byte {
	var b strings.Builder
	b.Write(v.id[:])
	now := v.now()
	for _, n := range v.ntas {
		if now.Before(n.ExpiresAt) {
			b.WriteString(n.Domain)
			b.WriteByte(0)
		}
	}
	return sha256.Sum256([]byte(b.String()))
}
