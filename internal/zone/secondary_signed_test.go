package zone

import (
	"crypto"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// These fixtures sign original immutable RRsets with a real ECDSA key. The
// production assembler does not use that private key or verify signatures.
type secondarySignedFixture struct {
	records []dns.RR
	key     *dns.DNSKEY
	signer  crypto.Signer
	now     time.Time
}

func signedSecondaryFixture(t *testing.T, nsec3 bool, extra ...dns.RR) *secondarySignedFixture {
	t.Helper()
	f := &secondarySignedFixture{now: time.Now().UTC().Truncate(time.Second)}
	f.key = &dns.DNSKEY{Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 300}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	private, err := f.key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	f.signer = private.(crypto.Signer)
	f.records = []dns.RR{mustSignedRR(t, "example.org. 300 IN SOA ns.example.org. admin.example.org. 4294967295 10 3 3600 60"), mustSignedRR(t, "example.org. 300 IN NS ns.example.org."), f.key, mustSignedRR(t, "ns.example.org. 300 IN A 192.0.2.53"), mustSignedRR(t, "www.example.org. 300 IN A 192.0.2.80"), mustSignedRR(t, "alias.example.org. 300 IN CNAME www.example.org."), mustSignedRR(t, "*.wild.example.org. 300 IN A 192.0.2.81"), mustSignedRR(t, "leaf.branch.example.org. 300 IN TXT data"), mustSignedRR(t, "dname.example.org. 300 IN DNAME example.net.")}
	f.records = append(f.records, extra...)
	if nsec3 {
		f.records = append(f.records, mustSignedRR(t, "example.org. 300 IN NSEC3PARAM 1 0 0 -"))
	}
	sets := map[secondaryRRKey][]dns.RR{}
	bitmaps := map[string][]uint16{}
	for _, rr := range f.records {
		key := secondaryRRKey{dns.CanonicalName(rr.Header().Name), rr.Header().Rrtype}
		sets[key] = append(sets[key], rr)
		bitmaps[key.name] = append(bitmaps[key.name], key.typ)
	}
	names := make([]string, 0, len(bitmaps))
	for name := range bitmaps {
		names = append(names, name)
	}
	if nsec3 {
		// NSEC3 includes empty nonterminals; their type bitmap is empty.
		names = append(names, "wild.example.org.", "branch.example.org.")
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(dns.HashName(a, 1, 0, ""), dns.HashName(b, 1, 0, "")) })
	} else {
		slices.SortFunc(names, func(a, b string) int {
			ka, _ := canonicalKey(a)
			kb, _ := canonicalKey(b)
			return strings.Compare(ka, kb)
		})
	}
	for i, name := range names {
		bits := slices.Clone(bitmaps[name])
		var denial dns.RR
		if nsec3 {
			if len(bits) > 0 {
				bits = append(bits, dns.TypeRRSIG)
			}
			slices.Sort(bits)
			denial = &dns.NSEC3{Hdr: dns.RR_Header{Name: dns.HashName(name, 1, 0, "") + ".example.org.", Rrtype: dns.TypeNSEC3, Class: dns.ClassINET, Ttl: 300}, Hash: 1, HashLength: 20, NextDomain: dns.HashName(names[(i+1)%len(names)], 1, 0, ""), TypeBitMap: bits}
		} else {
			bits = append(bits, dns.TypeRRSIG, dns.TypeNSEC)
			slices.Sort(bits)
			denial = &dns.NSEC{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 300}, NextDomain: names[(i+1)%len(names)], TypeBitMap: bits}
		}
		f.records = append(f.records, denial)
		key := secondaryRRKey{dns.CanonicalName(denial.Header().Name), denial.Header().Rrtype}
		sets[key] = []dns.RR{denial}
	}
	for _, set := range sets {
		f.records = append(f.records, f.sign(t, set))
	}
	return f
}
func (f *secondarySignedFixture) sign(t *testing.T, set []dns.RR) *dns.RRSIG {
	t.Helper()
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: set[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: set[0].Header().Ttl}, Algorithm: f.key.Algorithm, KeyTag: f.key.KeyTag(), SignerName: f.key.Hdr.Name, Inception: uint32(f.now.Add(-time.Hour).Unix()), Expiration: uint32(f.now.Add(time.Hour).Unix())} //nolint:gosec // Test fixture uses present RFC serial-time epoch.
	if err := sig.Sign(f.signer, set); err != nil {
		t.Fatal(err)
	}
	return sig
}
func mustSignedRR(t *testing.T, line string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(line)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}
func (f *secondarySignedFixture) build(t *testing.T) *Zone {
	t.Helper()
	dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeSecondary })
	z, err := buildSecondaryRaw(dz, f.records, f.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestSecondaryProofIndexPreservesRawSigningMaterial(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprint(n3), func(t *testing.T) {
			f := signedSecondaryFixture(t, n3)
			z := f.build(t)
			before := z.ContentHash()
			p, err := BuildSecondaryProofs(z)
			if err != nil {
				t.Fatal(err)
			}
			if p == nil || len(p.signatures) == 0 {
				t.Fatal("missing immutable signature index")
			}
			for key, sigs := range p.signatures {
				n := z.index[keyForSignedTest(t, key.name)]
				set := n.get(key.typ)
				if set == nil {
					t.Fatalf("signature without RRset %v", key)
				}
				for _, sig := range sigs {
					if err := sig.Verify(f.key, set.RRs); err != nil {
						t.Fatalf("raw signature changed for %v: %v", key, err)
					}
				}
			}
			if z.ContentHash() != before {
				t.Fatal("index construction changed raw zone")
			}
			if n3 {
				h := dns.HashName("www.example.org.", 1, 0, "")
				if p.nsec3ForHash(h, true) == nil || p.nsec3ForHash(h, false) != nil || p.nsec3ForHash(dns.HashName("missing.example.org.", 1, 0, ""), false) == nil {
					t.Fatal("NSEC3 exact/cover lookup")
				}
			} else {
				if p.exactNSEC("www.example.org.") == nil || p.coveringNSEC("www.example.org.") != nil || p.coveringNSEC("missing.example.org.") == nil || p.coveringNSEC("zzz.example.org.") == nil {
					t.Fatal("NSEC exact/canonical interval/wrap lookup")
				}
			}
		})
	}
}
func keyForSignedTest(t *testing.T, name string) string {
	t.Helper()
	k, err := canonicalKey(name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSecondaryProofIndexRejectsIncoherentRawDenial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n3     bool
		mutate func(*secondarySignedFixture)
	}{
		{"missing-key", false, func(f *secondarySignedFixture) {
			f.records = slices.DeleteFunc(f.records, func(rr dns.RR) bool { return rr.Header().Rrtype == dns.TypeDNSKEY })
		}},
		{"missing-apex-signature", false, func(f *secondarySignedFixture) {
			f.records = slices.DeleteFunc(f.records, func(rr dns.RR) bool { s, ok := rr.(*dns.RRSIG); return ok && s.TypeCovered == dns.TypeSOA })
		}},
		{"broken-ring", false, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if n, ok := rr.(*dns.NSEC); ok {
					n.NextDomain = "absent.example.org."
					break
				}
			}
		}},
		{"out-of-zone-next", false, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if n, ok := rr.(*dns.NSEC); ok {
					n.NextDomain = "other.test."
					break
				}
			}
		}},
		{"wrong-signer", false, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if s, ok := rr.(*dns.RRSIG); ok {
					s.SignerName = "other.test."
					break
				}
			}
		}},
		{"wrong-labels", false, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if s, ok := rr.(*dns.RRSIG); ok {
					s.Labels++
					break
				}
			}
		}},
		{"excessive-iterations", true, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if n, ok := rr.(*dns.NSEC3); ok {
					n.Iterations = 151
					break
				}
			}
		}},
		{"unknown-flags", true, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if n, ok := rr.(*dns.NSEC3); ok {
					n.Flags = 2
					break
				}
			}
		}},
		{"bad-hash", true, func(f *secondarySignedFixture) {
			for _, rr := range f.records {
				if n, ok := rr.(*dns.NSEC3); ok {
					n.NextDomain = "00000000000000000000000000000000"
					break
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := signedSecondaryFixture(t, tc.n3)
			tc.mutate(f)
			z := f.build(t)
			if _, err := BuildSecondaryProofs(z); err == nil {
				t.Fatal("accepted incoherent signing model")
			}
			dz := secondaryConfig()
			if published, err := BuildSecondary(dz, f.records, f.now.Add(time.Hour)); err == nil || published != nil {
				t.Fatal("factory published an incoherent signed model")
			}
		})
	}
}

func TestSecondarySignedAnswersVerifyAcrossPositiveWildcardAliasAndDenial(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprint(n3), func(t *testing.T) {
			f := signedSecondaryFixture(t, n3)
			z := f.build(t)
			p, err := BuildSecondaryProofs(z)
			if err != nil {
				t.Fatal(err)
			}
			rawHash := z.ContentHash()
			for _, tc := range []struct {
				name   string
				typ    uint16
				kind   Kind
				proofs bool
			}{
				{"www.example.org.", dns.TypeA, Success, false},
				{"alias.example.org.", dns.TypeA, Success, false},
				{"www.example.org.", dns.TypeAAAA, NoData, true},
				{"missing.example.org.", dns.TypeA, NXDomain, true},
				{"branch.example.org.", dns.TypeA, NoData, true},
				{"x.wild.example.org.", dns.TypeA, Success, true},
				{"x.wild.example.org.", dns.TypeAAAA, NoData, true},
				{"*.wild.example.org.", dns.TypeA, Success, false},
				{"x.dname.example.org.", dns.TypeA, CNAME, false},
				{"example.org.", dns.TypeDNSKEY, Success, false},
				{"www.example.org.", dns.TypeRRSIG, Success, false},
			} {
				t.Run(tc.name+dns.TypeToString[tc.typ], func(t *testing.T) {
					r, err := p.LookupWithin(tc.name, tc.typ, 0, nil, true, f.now)
					if err != nil {
						t.Fatal(err)
					}
					if r.Kind != tc.kind {
						t.Fatalf("kind %v want %v", r.Kind, tc.kind)
					}
					proofs := false
					for _, rr := range r.Ns {
						if rr.Header().Rrtype == dns.TypeNSEC || rr.Header().Rrtype == dns.TypeNSEC3 {
							proofs = true
						}
					}
					if proofs != tc.proofs {
						t.Fatalf("proof presence %t want %t: %v", proofs, tc.proofs, r.Ns)
					}
					verifySecondarySections(t, f.key, r.Answer, r.Ns)
					unsigned, err := p.LookupWithin(tc.name, tc.typ, 0, nil, false, f.now)
					if err != nil {
						t.Fatal(err)
					}
					for _, section := range [][]dns.RR{unsigned.Answer, unsigned.Ns, unsigned.Extra} {
						for _, rr := range section {
							if secondaryDNSSECType(rr.Header().Rrtype) && rr.Header().Rrtype != tc.typ {
								t.Fatal("DO=0 leaked unsolicited DNSSEC data")
							}
						}
					}
				})
			}
			if z.ContentHash() != rawHash {
				t.Fatal("proof assembly changed raw immutable data")
			}
		})
	}
}

func verifySecondarySections(t *testing.T, key *dns.DNSKEY, sections ...[]dns.RR) {
	t.Helper()
	for _, section := range sections {
		groups := map[secondaryRRKey][]dns.RR{}
		for _, rr := range section {
			if rr.Header().Rrtype != dns.TypeRRSIG {
				k := secondaryRRKey{dns.CanonicalName(rr.Header().Name), rr.Header().Rrtype}
				groups[k] = append(groups[k], rr)
			}
		}
		for _, rr := range section {
			if sig, ok := rr.(*dns.RRSIG); ok {
				set := groups[secondaryRRKey{dns.CanonicalName(sig.Hdr.Name), sig.TypeCovered}]
				if len(set) == 0 {
					if len(section) > 0 && section[0].Header().Rrtype == dns.TypeRRSIG {
						continue
					}
					t.Fatalf("orphan response signature %s", sig)
				}
				if err := sig.Verify(key, set); err != nil {
					t.Fatalf("response signature no longer verifies %s: %v", sig, err)
				}
			}
		}
	}
}

func TestSecondarySignatureLivenessDiagnosticAndTTLPreservation(t *testing.T) {
	f := signedSecondaryFixture(t, false)
	z := f.build(t)
	p, err := BuildSecondaryProofs(z)
	if err != nil {
		t.Fatal(err)
	}
	expiry := f.now.Add(time.Hour)
	r, err := p.LookupWithin("www.example.org.", dns.TypeA, 0, nil, true, expiry.Add(-1500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range r.Answer {
		if rr.Header().Ttl != 1 {
			t.Fatalf("response exceeds validity/zone deadline: %s", rr)
		}
	}
	before := z.ContentHash()
	r.Answer[0].Header().Ttl = 999
	if z.index[keyForProof("www.example.org.")].get(dns.TypeA).TTL != 300 || z.ContentHash() != before {
		t.Fatal("response mutated immutable RRset")
	}
	// Extend only the fixture's zone expiry; transferred signatures stay old.
	dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeSecondary })
	z, err = BuildSecondary(dz, f.records, expiry.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err = BuildSecondaryProofs(z)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.LookupWithin("www.example.org.", dns.TypeA, 0, nil, true, expiry); err == nil {
		t.Fatal("expired required signature served")
	}
	if _, err := p.LookupWithin("www.example.org.", dns.TypeA, 0, nil, false, expiry); err != nil {
		t.Fatal("DO0 ordinary data became unavailable")
	}
	for _, typ := range []uint16{dns.TypeRRSIG, dns.TypeDNSKEY, dns.TypeNSEC} {
		name := "www.example.org."
		if typ == dns.TypeDNSKEY {
			name = "example.org."
		}
		r, err := p.LookupWithin(name, typ, 0, nil, true, expiry)
		if err != nil || len(r.Answer) == 0 {
			t.Fatalf("diagnostic %s lost raw data: %v", dns.TypeToString[typ], err)
		}
	}
	// AXFR retains original TTL/signature bytes regardless of response caps.
	for chunk := range z.AXFR(DefaultChunkBytes) {
		for _, rr := range chunk {
			if rr.Header().Ttl != 300 {
				t.Fatalf("rendering altered transfer TTL: %s", rr)
			}
		}
	}
}

func TestSecondarySignatureSerialTimeAndFractionalTTL(t *testing.T) {
	sig := &dns.RRSIG{Inception: 4294967280, Expiration: 15}
	// RFC timestamps wrap at 2^32. A live signature straddling the wrap is
	// represented by its delta rather than treating expiration as an old date.
	at := time.Unix(4294967296+5, 500000000)
	if !secondarySignatureLive(sig, at) || secondarySignatureLive(sig, time.Unix(4294967296+15, 0)) || secondarySignatureLive(sig, time.Unix(4294967280-1, 0)) {
		t.Fatal("signature serial-time wrap mishandled")
	}
	a := secondaryAssembly{now: at}
	if a.signatureTTL(sig) != 9 {
		t.Fatal("fractional TTL extended signature validity")
	}
	sig.Expiration = sig.Inception + 1<<31
	if secondarySignatureLive(sig, at) {
		t.Fatal("ambiguous half-range signature accepted")
	}
}

func TestSecondarySignedReferralDSAndInsecureProof(t *testing.T) {
	// Add signed and unsigned delegations to the authentic fixture, rebuild
	// its complete denial ring and signatures as a test-side primary would.
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprint(n3), func(t *testing.T) {
			f := signedSecondaryFixture(t, n3)
			// Keep ordinary raw data, removing the fixture's old derived proof layer.
			f.records = slices.DeleteFunc(f.records, func(rr dns.RR) bool {
				return rr.Header().Rrtype == dns.TypeRRSIG || rr.Header().Rrtype == dns.TypeNSEC || rr.Header().Rrtype == dns.TypeNSEC3
			})
			f.records = append(f.records, mustSignedRR(t, "insecure.example.org. 300 IN NS ns.insecure.example.org."), mustSignedRR(t, "ns.insecure.example.org. 300 IN A 192.0.2.54"), mustSignedRR(t, "secure.example.org. 300 IN NS ns.secure.example.org."), mustSignedRR(t, "ns.secure.example.org. 300 IN A 192.0.2.55"), mustSignedRR(t, "secure.example.org. 300 IN DS 12345 13 2 00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF"))
			rebuildSignedFixtureDenial(t, f, n3, false)
			z := f.build(t)
			p, err := BuildSecondaryProofs(z)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"insecure.example.org.", "secure.example.org.", "www.insecure.example.org."} {
				r, err := p.LookupWithin(name, dns.TypeA, 0, nil, true, f.now)
				if err != nil {
					t.Fatal(err)
				}
				if r.Kind != Delegation || len(r.Extra) != 1 {
					t.Fatalf("referral %+v", r)
				}
				verifySecondarySections(t, f.key, r.Ns)
				ds := false
				proof := false
				nsSig := false
				for _, rr := range r.Ns {
					switch v := rr.(type) {
					case *dns.DS:
						ds = true
					case *dns.NSEC, *dns.NSEC3:
						proof = true
					case *dns.RRSIG:
						nsSig = nsSig || v.TypeCovered == dns.TypeNS
					}
				}
				if nsSig || (name == "secure.example.org.") != ds || proof == (name == "secure.example.org.") {
					t.Fatalf("delegation evidence %+v", r.Ns)
				}
			}
			for _, name := range []string{"insecure.example.org.", "secure.example.org."} {
				r, err := p.LookupWithin(name, dns.TypeDS, 0, nil, true, f.now)
				if err != nil {
					t.Fatal(err)
				}
				want := NoData
				if name == "secure.example.org." {
					want = Success
				}
				if r.Kind != want {
					t.Fatalf("parent DS %+v", r)
				}
				verifySecondarySections(t, f.key, r.Answer, r.Ns)
			}
		})
	}
}

func rebuildSignedFixtureDenial(t *testing.T, f *secondarySignedFixture, n3, optout bool) {
	t.Helper()
	sets := map[secondaryRRKey][]dns.RR{}
	bits := map[string][]uint16{}
	glue := func(name string) bool {
		return dns.IsSubDomain("insecure.example.org.", name) && name != "insecure.example.org." || dns.IsSubDomain("secure.example.org.", name) && name != "secure.example.org."
	}
	for _, rr := range f.records {
		k := secondaryRRKey{dns.CanonicalName(rr.Header().Name), rr.Header().Rrtype}
		sets[k] = append(sets[k], rr)
		if !glue(k.name) {
			bits[k.name] = append(bits[k.name], k.typ)
		}
	}
	var names []string
	for name := range bits {
		if !n3 || !optout || name != "insecure.example.org." {
			names = append(names, name)
		}
	}
	if n3 {
		names = append(names, "branch.example.org.", "wild.example.org.")
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(dns.HashName(a, 1, 0, ""), dns.HashName(b, 1, 0, "")) })
	} else {
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(keyForProof(a), keyForProof(b)) })
	}
	for i, name := range names {
		bitmap := slices.Clone(bits[name])
		var rr dns.RR
		cut := name == "insecure.example.org." || name == "secure.example.org."
		if n3 {
			if len(bitmap) > 0 && !cut {
				bitmap = append(bitmap, dns.TypeRRSIG)
			}
			if name == "secure.example.org." {
				bitmap = append(bitmap, dns.TypeRRSIG)
			}
			slices.Sort(bitmap)
			flags := uint8(0)
			if optout {
				flags = 1
			}
			rr = &dns.NSEC3{Hdr: dns.RR_Header{Name: dns.HashName(name, 1, 0, "") + ".example.org.", Rrtype: dns.TypeNSEC3, Class: dns.ClassINET, Ttl: 300}, Hash: 1, HashLength: 20, Flags: flags, NextDomain: dns.HashName(names[(i+1)%len(names)], 1, 0, ""), TypeBitMap: bitmap}
		} else {
			bitmap = append(bitmap, dns.TypeRRSIG, dns.TypeNSEC)
			slices.Sort(bitmap)
			rr = &dns.NSEC{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 300}, NextDomain: names[(i+1)%len(names)], TypeBitMap: bitmap}
		}
		f.records = append(f.records, rr)
		sets[secondaryRRKey{dns.CanonicalName(rr.Header().Name), rr.Header().Rrtype}] = []dns.RR{rr}
	}
	for k, set := range sets {
		if glue(k.name) || (k.typ == dns.TypeNS && (k.name == "insecure.example.org." || k.name == "secure.example.org.")) {
			continue
		}
		f.records = append(f.records, f.sign(t, set))
	}
}

func TestSecondaryNSEC3OptOutDelegation(t *testing.T) {
	f := signedSecondaryFixture(t, true)
	f.records = slices.DeleteFunc(f.records, func(rr dns.RR) bool {
		return rr.Header().Rrtype == dns.TypeRRSIG || rr.Header().Rrtype == dns.TypeNSEC3
	})
	f.records = append(f.records, mustSignedRR(t, "insecure.example.org. 300 IN NS ns.insecure.example.org."), mustSignedRR(t, "ns.insecure.example.org. 300 IN A 192.0.2.54"))
	rebuildSignedFixtureDenial(t, f, true, true)
	p, err := BuildSecondaryProofs(f.build(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []uint16{dns.TypeA, dns.TypeDS} {
		r, err := p.LookupWithin("insecure.example.org.", typ, 0, nil, true, f.now)
		if err != nil {
			t.Fatal(err)
		}
		want := Delegation
		if typ == dns.TypeDS {
			want = NoData
		}
		if r.Kind != want {
			t.Fatal(r)
		}
		verifySecondarySections(t, f.key, r.Ns)
		hasOptout := false
		for _, rr := range r.Ns {
			if n, ok := rr.(*dns.NSEC3); ok && n.Flags&1 != 0 {
				hasOptout = true
			}
		}
		if !hasOptout {
			t.Fatal("opt-out delegation proof missing")
		}
	}
}

func TestSecondaryWildcardAliasProofAndDNAMEClamping(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprint(n3), func(t *testing.T) {
			f := signedSecondaryFixture(t, n3)
			f.records = slices.DeleteFunc(f.records, func(rr dns.RR) bool {
				return rr.Header().Rrtype == dns.TypeRRSIG || rr.Header().Rrtype == dns.TypeNSEC || rr.Header().Rrtype == dns.TypeNSEC3 || dns.CanonicalName(rr.Header().Name) == "*.wild.example.org."
			})
			f.records = append(f.records, mustSignedRR(t, "*.wild.example.org. 300 IN CNAME www.example.org."))
			rebuildSignedFixtureDenial(t, f, n3, false)
			p, err := BuildSecondaryProofs(f.build(t))
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.LookupWithin("x.wild.example.org.", dns.TypeA, 0, nil, true, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Success || !r.Wildcard || len(r.Ns) == 0 {
				t.Fatalf("wildcard alias %+v", r)
			}
			verifySecondarySections(t, f.key, r.Answer, r.Ns)
			r, err = p.LookupWithin("x.dname.example.org.", dns.TypeA, 0, nil, true, f.now.Add(time.Hour-10*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			for _, rr := range r.Answer {
				if rr.Header().Ttl != 10 {
					t.Fatalf("synthesized DNAME alias TTL differs from authenticated DNAME: %s", rr)
				}
			}
		})
	}
}

func TestSecondarySignedPublishedResponseBudget(t *testing.T) {
	var extra []dns.RR
	for typ := uint16(40000); typ < 40070; typ++ {
		extra = append(extra, mustSignedRR(t, fmt.Sprintf("example.org. 300 IN TYPE%d \\# 1 ff", typ)))
	}
	f := signedSecondaryFixture(t, false, extra...)
	z, err := BuildSecondary(secondaryConfig(), f.records, f.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z.LookupSignedWithin("example.org.", dns.TypeANY, 0, nil, true, f.now); err == nil {
		t.Fatal("oversized signed ANY bypassed RRset budget")
	}
	if r, err := z.LookupSignedWithin("www.example.org.", dns.TypeA, 0, nil, true, f.now); err != nil || len(r.Answer) != 2 {
		t.Fatalf("bounded ordinary answer was affected: %v %v", r, err)
	}
}

func TestSecondarySignedLargeRRsetCannotBypassByteBudget(t *testing.T) {
	var extra []dns.RR
	for i := range 280 {
		rr := &dns.TXT{Hdr: dns.RR_Header{Name: "large.example.org.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 300}}
		for range 16 {
			rr.Txt = append(rr.Txt, strings.Repeat("x", 250))
		}
		rr.Txt = append(rr.Txt, fmt.Sprint(i))
		extra = append(extra, rr)
	}
	f := signedSecondaryFixture(t, false, extra...)
	z, err := BuildSecondary(secondaryConfig(), f.records, f.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, do := range []bool{false, true} {
		if _, err := z.LookupSignedWithin("large.example.org.", dns.TypeTXT, 0, nil, do, f.now); err == nil {
			t.Fatal("large RRset bypassed byte budget")
		}
	}
}

func TestSecondarySignedSignatureCountBound(t *testing.T) {
	f := signedSecondaryFixture(t, false)
	for _, rr := range f.records {
		if sig, ok := rr.(*dns.RRSIG); ok && sig.TypeCovered == dns.TypeSOA {
			for i := uint16(1); i <= 16; i++ {
				duplicate := dns.Copy(sig).(*dns.RRSIG)
				duplicate.KeyTag += i
				f.records = append(f.records, duplicate)
			}
			break
		}
	}
	if z, err := BuildSecondary(secondaryConfig(), f.records, f.now.Add(time.Hour)); err == nil || z != nil {
		t.Fatal("unbounded covering signature set was published")
	}
}
