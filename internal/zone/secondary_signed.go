package zone

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

const (
	secondaryMaxSignatures      = 16
	secondaryMaxProofRRsets     = 64
	secondaryMaxProofBytes      = 1 << 20
	secondaryMaxNSEC3Iterations = 150
	secondaryMaxNSEC3HashWork   = 4096
)

type secondaryRRKey struct {
	name string
	typ  uint16
}
type secondaryDenial struct {
	key string
	rr  dns.RR
}

// SecondaryProofs is a build-time immutable index over transferred signing
// material. It describes proof assembly, never local DNSSEC authentication.
// Cryptographic signature verification and chain-of-trust evaluation belong
// to the recursive validator/client, not this authoritative serving model.
type SecondaryProofs struct {
	z          *Zone
	signatures map[secondaryRRKey][]*dns.RRSIG
	nsec       []secondaryDenial
	nsec3      []secondaryDenial
	params     *dns.NSEC3
	hashes     map[string]string
}

// BuildSecondaryProofs prepares bounded signature and canonical denial indices.
// It never changes raw zone RRs. A nil index means an ordinary unsigned zone.
// Snapshot owners must call this once before publication, not on queries.
func BuildSecondaryProofs(z *Zone) (*SecondaryProofs, error) {
	if z == nil || z.typ != v1alpha1.ZoneTypeSecondary || z.soa == nil {
		return nil, fmt.Errorf("received secondary data is required")
	}
	if !z.hasSignatures {
		return nil, nil
	}
	p := &SecondaryProofs{z: z, signatures: map[secondaryRRKey][]*dns.RRSIG{}, hashes: map[string]string{}}
	var failure error
	z.RRsets(func(set *RRset) bool {
		owner := dns.CanonicalName(set.Name)
		switch set.Type {
		case dns.TypeRRSIG:
			for _, rr := range set.RRs {
				sig := rr.(*dns.RRSIG)
				labels := dns.CountLabel(owner)
				if strings.HasPrefix(owner, "*.") {
					labels--
				}
				if dns.CanonicalName(sig.SignerName) != z.name || int(sig.Labels) != labels || sig.TypeCovered == dns.TypeRRSIG || len(sig.Signature) == 0 {
					failure = fmt.Errorf("inapplicable transferred RRSIG at %s", owner)
					return false
				}
				key := secondaryRRKey{owner, sig.TypeCovered}
				p.signatures[key] = append(p.signatures[key], sig)
				if len(p.signatures[key]) > secondaryMaxSignatures {
					failure = fmt.Errorf("transferred RRset exceeds signature bound")
					return false
				}
			}
		case dns.TypeNSEC:
			if len(set.RRs) != 1 {
				failure = fmt.Errorf("multiple NSEC records at %s", owner)
				return false
			}
			n := set.RRs[0].(*dns.NSEC)
			next, err := canonicalKey(n.NextDomain)
			if err != nil || !dns.IsFqdn(n.NextDomain) || !isKeyPrefix(next, z.key) || !validSecondaryBitmap(n.TypeBitMap) {
				failure = fmt.Errorf("invalid transferred NSEC at %s", owner)
				return false
			}
			key, _ := canonicalKey(owner)
			p.nsec = append(p.nsec, secondaryDenial{key, n})
		case dns.TypeNSEC3:
			if len(set.RRs) != 1 {
				failure = fmt.Errorf("multiple NSEC3 records at %s", owner)
				return false
			}
			n := set.RRs[0].(*dns.NSEC3)
			labels := dns.SplitDomainName(owner)
			salt, err := hex.DecodeString(n.Salt)
			if n.Hash != 1 || n.Flags & ^uint8(1) != 0 || n.Iterations > secondaryMaxNSEC3Iterations || err != nil || len(salt) > 32 || len(labels) < 1 || dns.CanonicalName(dns.Fqdn(strings.Join(labels[1:], "."))) != z.name || !validSecondaryHash(labels[0]) || !validSecondaryHash(n.NextDomain) || !validSecondaryBitmap(n.TypeBitMap) {
				failure = fmt.Errorf("unsupported or malformed transferred NSEC3 parameters at %s", owner)
				return false
			}
			if p.params != nil && (p.params.Hash != n.Hash || p.params.Iterations != n.Iterations || !strings.EqualFold(p.params.Salt, n.Salt)) {
				failure = fmt.Errorf("multiple NSEC3 parameter chains are unsupported")
				return false
			}
			p.params = n
			p.nsec3 = append(p.nsec3, secondaryDenial{strings.ToUpper(labels[0]), n})
		}
		return true
	})
	if failure != nil {
		return nil, failure
	}
	if len(p.signatures) == 0 {
		return nil, fmt.Errorf("legacy-only signed secondary proof assembly is unsupported")
	}
	if z.apex.get(dns.TypeDNSKEY) == nil {
		return nil, fmt.Errorf("signed secondary requires apex DNSKEY")
	}
	if (len(p.nsec) > 0) == (len(p.nsec3) > 0) {
		return nil, fmt.Errorf("signed secondary requires one NSEC or NSEC3 chain")
	}
	for _, t := range []uint16{dns.TypeSOA, dns.TypeNS, dns.TypeDNSKEY} {
		if len(p.signatures[secondaryRRKey{z.name, t}]) == 0 {
			return nil, fmt.Errorf("missing apex %s signature", dns.TypeToString[t])
		}
	}
	for _, chain := range [][]secondaryDenial{p.nsec, p.nsec3} {
		slices.SortFunc(chain, func(a, b secondaryDenial) int { return strings.Compare(a.key, b.key) })
		for i, d := range chain {
			var next string
			switch rr := d.rr.(type) {
			case *dns.NSEC:
				next, _ = canonicalKey(rr.NextDomain)
			case *dns.NSEC3:
				next = strings.ToUpper(rr.NextDomain)
			}
			if next != chain[(i+1)%len(chain)].key {
				return nil, fmt.Errorf("incomplete or inconsistent transferred denial ring")
			}
			if len(p.signatures[secondaryRRKey{dns.CanonicalName(d.rr.Header().Name), d.rr.Header().Rrtype}]) == 0 {
				return nil, fmt.Errorf("missing transferred denial signature")
			}
		}
	}
	if p.params != nil {
		z.tree.walk(func(n *node) bool {
			// NSEC3 hash owners and occluded glue are not authoritative names.
			if n.get(dns.TypeNSEC3) != nil || !secondaryAuthoritativeNode(z, n) {
				return true
			}
			p.hashes[n.key] = strings.ToUpper(dns.HashName(n.name, p.params.Hash, p.params.Iterations, p.params.Salt))
			return true
		})
		if param := z.apex.get(dns.TypeNSEC3PARAM); param != nil {
			if len(param.RRs) != 1 {
				return nil, fmt.Errorf("multiple NSEC3PARAM chains are unsupported")
			}
			rr := param.RRs[0].(*dns.NSEC3PARAM)
			if rr.Hash != p.params.Hash || rr.Iterations != p.params.Iterations || !strings.EqualFold(rr.Salt, p.params.Salt) || rr.Flags != 0 {
				return nil, fmt.Errorf("NSEC3PARAM does not match transferred chain")
			}
		}
	}
	// Every ordinary authoritative RRset requires stored signature material;
	// delegation NS and occluded glue do not acquire parent authentication.
	var structural error
	z.tree.walk(func(n *node) bool {
		if !secondaryAuthoritativeNode(z, n) {
			return true
		}
		atCut := n != z.apex && n.get(dns.TypeNS) != nil
		for _, set := range n.sets {
			if set.Type == dns.TypeRRSIG || set.Type == dns.TypeSIG || (atCut && set.Type != dns.TypeDS && set.Type != dns.TypeNSEC) {
				continue
			}
			if len(p.signatures[secondaryRRKey{n.name, set.Type}]) == 0 {
				structural = fmt.Errorf("missing transferred signature at %s for %s", n.name, dns.TypeToString[set.Type])
				return false
			}
		}
		if len(p.nsec) > 0 {
			if n.empty() {
				return true
			}
			denial := p.exactNSEC(n.name)
			if denial == nil {
				structural = fmt.Errorf("incomplete NSEC authoritative-name coverage")
				return false
			}
			expected := []uint16{}
			for _, set := range n.sets {
				if !atCut || secondaryParentType(set.Type) {
					expected = append(expected, set.Type)
				}
			}
			slices.Sort(expected)
			if !slices.Equal(expected, denial.TypeBitMap) {
				structural = fmt.Errorf("NSEC bitmap contradicts transferred RRsets at %s", n.name)
				return false
			}
		} else if n.get(dns.TypeNSEC3) == nil {
			hash := p.hashes[n.key]
			denial := p.nsec3ForHash(hash, true)
			if denial == nil {
				// Only insecure delegation names may be omitted from an opt-out ring.
				if atCut && n.get(dns.TypeDS) == nil {
					cover := p.nsec3ForHash(hash, false)
					if cover != nil && cover.Flags&1 != 0 {
						return true
					}
				}
				structural = fmt.Errorf("incomplete NSEC3 authoritative-name coverage at %s", n.name)
				return false
			}
			expected := []uint16{}
			for _, set := range n.sets {
				if !atCut || secondaryParentType(set.Type) {
					expected = append(expected, set.Type)
				}
			}
			slices.Sort(expected)
			if !slices.Equal(expected, denial.TypeBitMap) {
				structural = fmt.Errorf("NSEC3 bitmap contradicts transferred RRsets at %s", n.name)
				return false
			}
		}
		return true
	})
	if structural != nil {
		return nil, structural
	}
	for key := range p.signatures {
		n := z.index[keyForProof(key.name)]
		if n == nil || n.get(key.typ) == nil {
			return nil, fmt.Errorf("orphan transferred RRSIG")
		}
	}
	return p, nil
}

func secondaryAuthoritativeNode(z *Zone, n *node) bool {
	key := strings.TrimSuffix(n.key, "\x00")
	for {
		pos := strings.LastIndexByte(key, keySep)
		if pos < 0 {
			return true
		}
		key = key[:pos+1]
		if ancestor := z.index[key]; ancestor != nil && ancestor != z.apex && (ancestor.get(dns.TypeNS) != nil || ancestor.get(dns.TypeDNAME) != nil) {
			return false
		}
		if key == z.key {
			return true
		}
		key = strings.TrimSuffix(key, "\x00")
	}
}

func validSecondaryHash(value string) bool {
	b, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
	return err == nil && len(b) == 20 && len(value) == 32
}
func validSecondaryBitmap(bits []uint16) bool {
	for i, t := range bits {
		if t == dns.TypeNone || secondaryPseudoType(t) || (i > 0 && bits[i-1] >= t) {
			return false
		}
	}
	return true
}

func (p *SecondaryProofs) exactNSEC(name string) *dns.NSEC {
	key, err := canonicalKey(name)
	if err != nil {
		return nil
	}
	i, ok := slices.BinarySearchFunc(p.nsec, key, func(d secondaryDenial, k string) int { return strings.Compare(d.key, k) })
	if !ok {
		return nil
	}
	return p.nsec[i].rr.(*dns.NSEC)
}
func (p *SecondaryProofs) coveringNSEC(name string) *dns.NSEC {
	key, err := canonicalKey(name)
	if err != nil || len(p.nsec) == 0 {
		return nil
	}
	i, exact := slices.BinarySearchFunc(p.nsec, key, func(d secondaryDenial, k string) int { return strings.Compare(d.key, k) })
	if exact {
		return nil
	}
	i = (i + len(p.nsec) - 1) % len(p.nsec)
	rr := p.nsec[i].rr.(*dns.NSEC)
	next, _ := canonicalKey(rr.NextDomain)
	if key == next {
		return nil
	}
	return rr
}
func (p *SecondaryProofs) nsec3ForHash(hash string, exact bool) *dns.NSEC3 {
	if len(p.nsec3) == 0 {
		return nil
	}
	i, found := slices.BinarySearchFunc(p.nsec3, strings.ToUpper(hash), func(d secondaryDenial, h string) int { return strings.Compare(d.key, h) })
	if exact {
		if !found {
			return nil
		}
		return p.nsec3[i].rr.(*dns.NSEC3)
	}
	if found {
		return nil
	}
	i = (i + len(p.nsec3) - 1) % len(p.nsec3)
	return p.nsec3[i].rr.(*dns.NSEC3)
}

// At a delegation only parent-side NS/DS/denial metadata is authoritative.
// Addresses even at the cut owner are glue and absent from parent bitmaps.
func secondaryParentType(typ uint16) bool {
	return typ == dns.TypeNS || typ == dns.TypeDS || typ == dns.TypeNSEC || typ == dns.TypeRRSIG
}
