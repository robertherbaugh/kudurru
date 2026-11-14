package dnssec

import (
	"context"
	"crypto"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// All DNS fixtures are generated here, including signatures and DS chains.
// No private operational key or third-party implementation is embedded.
type signedZone struct {
	key    *dns.DNSKEY
	signer crypto.Signer
}
type fixture struct {
	t           *testing.T
	now         time.Time
	root, child signedZone
	replies     map[rrkey]*dns.Msg
	calls       int
}

func (f *fixture) rr(text string) dns.RR {
	f.t.Helper()
	r, err := dns.NewRR(text)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), replies: map[rrkey]*dns.Msg{}}
	makeZone := func(name string) signedZone {
		k := &dns.DNSKEY{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
		priv, err := k.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		return signedZone{k, priv.(crypto.Signer)}
	}
	f.root, f.child = makeZone("."), makeZone("example.")
	f.replies[rrkey{".", dns.TypeDNSKEY}] = f.reply(".", dns.TypeDNSKEY, dns.RcodeSuccess, f.sign(f.root, f.root.key), nil)
	f.replies[rrkey{"example.", dns.TypeDNSKEY}] = f.reply("example.", dns.TypeDNSKEY, dns.RcodeSuccess, f.sign(f.child, f.child.key), nil)
	ds := f.child.key.ToDS(dns.SHA256)
	ds.Hdr.Ttl = 3600
	f.replies[rrkey{"example.", dns.TypeDS}] = f.reply("example.", dns.TypeDS, dns.RcodeSuccess, f.sign(f.root, ds), nil)
	return f
}
func (f *fixture) sign(zone signedZone, rrs ...dns.RR) []dns.RR {
	f.t.Helper()
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: rrs[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: rrs[0].Header().Ttl}, Algorithm: zone.key.Algorithm, KeyTag: zone.key.KeyTag(), SignerName: zone.key.Hdr.Name, Inception: uint32(f.now.Add(-time.Hour).Unix() & 0xffffffff), Expiration: uint32(f.now.Add(2*time.Hour).Unix() & 0xffffffff)}
	if err := sig.Sign(zone.signer, rrs); err != nil {
		f.t.Fatal(err)
	}
	return append(append([]dns.RR(nil), rrs...), sig)
}
func (f *fixture) reply(name string, typ uint16, rcode int, answer, ns []dns.RR) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(name, typ)
	q.SetEdns0(1232, true)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Rcode = rcode
	m.Answer, m.Ns = answer, ns
	return m
}
func (f *fixture) lookup(ctx context.Context, name string, typ uint16) (*dns.Msg, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m := f.replies[rrkey{dns.CanonicalName(name), typ}]; m != nil {
		return m.Copy(), nil
	}
	return nil, fmt.Errorf("unexpected proof query %s %s", name, dns.TypeToString[typ])
}
func (f *fixture) validator(opts Options) *Validator {
	f.t.Helper()
	if opts.Anchors == nil {
		opts.Anchors = []dns.RR{f.root.key.ToDS(dns.SHA256)}
	}
	opts.Now = func() time.Time { return f.now }
	v, err := New(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}
func (f *fixture) soa() dns.RR {
	return f.rr("example. 300 IN SOA ns.example. hostmaster.example. 1 3600 600 86400 300")
}
func (f *fixture) positive(name string) *dns.Msg {
	return f.reply(name, dns.TypeA, dns.RcodeSuccess, f.sign(f.child, f.rr(name+" 60 IN A 192.0.2.1")), nil)
}
func (f *fixture) check(v *Validator, m *dns.Msg, want pipeline.ValidationState) Result {
	f.t.Helper()
	before := m.String()
	r, err := v.Validate(context.Background(), m.Question[0], m, f.lookup)
	if r.Security.State != want || (want == pipeline.ValidationSecure || want == pipeline.ValidationInsecure) && err != nil {
		f.t.Fatalf("verdict %s, want %s: %v %s", r.Security.State, want, err, r.Security.Reason)
	}
	if m.String() != before {
		f.t.Fatal("validator mutated input")
	}
	return r
}

func TestPositiveTrustChainAliasesAndTTL(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	m := f.positive("www.example.")
	m.AuthenticatedData = true
	r := f.check(v, m, pipeline.ValidationSecure)
	if r.Msg.AuthenticatedData || f.calls != 3 || r.Msg.Answer[0].Header().Ttl != 60 || r.Security.ValidUntil.IsZero() {
		t.Fatalf("provenance: %+v calls=%d", r.Security, f.calls)
	}
	// A short data TTL is not a signature deadline: stale answering remains
	// possible while the key/DS chain and signature are valid.
	if !r.Security.AvailableAt(f.now.Add(61 * time.Second)) {
		t.Fatal("answer TTL incorrectly expires signature provenance")
	}
	alias := f.reply("alias.example.", dns.TypeA, 0, append(f.sign(f.child, f.rr("alias.example. 30 IN CNAME www.example.")), m.Answer...), nil)
	f.check(v, alias, pipeline.ValidationSecure)
	for _, typ := range []uint16{dns.TypeDNSKEY, dns.TypeDS} {
		f.check(v, f.replies[rrkey{"example.", typ}], pipeline.ValidationSecure)
	}
	// Received TTL cannot extend the signature's authenticated original TTL.
	m.Answer[0].Header().Ttl = 600
	r = f.check(v, m, pipeline.ValidationSecure)
	if r.Msg.Answer[0].Header().Ttl != 60 {
		t.Fatal("Original TTL not enforced")
	}
}

func TestSignatureExactExpiryAndValidationCompletion(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	m := f.positive("www.example.")
	expiry := f.now.Add(2 * time.Hour)
	f.now = expiry.Add(-1500 * time.Millisecond)
	s := &validation{v: v, ctx: context.Background(), now: f.now}
	deadline, err := s.verify(m.Answer[:1], []*dns.RRSIG{m.Answer[1].(*dns.RRSIG)}, []*dns.DNSKEY{f.child.key})
	if err != nil || !deadline.Equal(expiry) {
		t.Fatalf("signature deadline: %s %v", deadline, err)
	}
	r := f.check(v, m, pipeline.ValidationSecure)
	if r.Security.ValidUntil.After(expiry) || r.Msg.Answer[0].Header().Ttl != 1 {
		t.Fatalf("fractional lifetime extended: %+v ttl=%d", r.Security, r.Msg.Answer[0].Header().Ttl)
	}
	f.now = expiry
	f.check(v, m, pipeline.ValidationBogus)
	f.now = expiry.Add(-time.Second)
	lookup := func(ctx context.Context, name string, typ uint16) (*dns.Msg, error) {
		reply, err := f.lookup(ctx, name, typ)
		f.now = expiry
		return reply, err
	}
	r, err = v.Validate(context.Background(), m.Question[0], m, lookup)
	if err == nil || r.Security.State != pipeline.ValidationBogus || r.Security.Reason != "validation_expired" {
		t.Fatalf("expired during lookup: %+v %v", r.Security, err)
	}
}

func TestLiteralWildcardAndMalformedProof(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	m := f.reply("*.example.", dns.TypeA, 0, f.sign(f.child, f.rr("*.example. 60 IN A 192.0.2.1")), nil)
	f.check(v, m, pipeline.ValidationSecure)
	var nilKey *dns.DNSKEY
	for _, rr := range []dns.RR{nil, nilKey} {
		answer := f.positive("www.example.")
		answer.Answer = append(answer.Answer, rr)
		r, err := v.Validate(context.Background(), answer.Question[0], answer, f.lookup)
		if err == nil || r.Security.State != pipeline.ValidationBogus {
			t.Fatalf("nil answer accepted: %v", err)
		}
		lookup := func(_ context.Context, name string, typ uint16) (*dns.Msg, error) {
			return f.reply(name, typ, 0, []dns.RR{rr}, nil), nil
		}
		answer = f.positive("www.example.")
		r, err = v.Validate(context.Background(), answer.Question[0], answer, lookup)
		if err == nil || r.Security.State != pipeline.ValidationBogus {
			t.Fatalf("nil proof accepted: %v", err)
		}
	}
	for _, hash := range []string{strings.Repeat("X", 32), strings.Repeat("!", 32), strings.Repeat("0", 31)} {
		if validHash(hash) {
			t.Fatalf("invalid hash accepted: %s", hash)
		}
	}
}

func TestBogusSignaturesAndChain(t *testing.T) {
	for _, kind := range []string{"data", "missing", "expired", "future", "ds", "selfsigned", "loop", "unrelated", "class"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			v := f.validator(Options{})
			m := f.positive("www.example.")
			switch kind {
			case "data":
				m.Answer[0].(*dns.A).A[3] = 2
			case "missing":
				m.Answer = m.Answer[:1]
				f.replies[rrkey{"www.example.", dns.TypeSOA}] = f.reply("www.example.", dns.TypeSOA, 0, nil, f.sign(f.child, f.soa()))
			case "expired":
				f.now = f.now.Add(2 * time.Hour)
			case "future":
				f.now = f.now.Add(-2 * time.Hour)
			case "ds":
				ds := dns.Copy(f.replies[rrkey{"example.", dns.TypeDS}].Answer[0]).(*dns.DS)
				ds.Digest = strings.Repeat("00", 32)
				f.replies[rrkey{"example.", dns.TypeDS}].Answer = f.sign(f.root, ds)
			case "selfsigned":
				fake := newFixture(t)
				m.Answer = fake.positive("www.example.").Answer
			case "loop":
				m.Answer = f.sign(f.child, f.rr("www.example. 60 IN CNAME www.example."))
			case "unrelated":
				m.Answer = append(m.Answer, f.sign(f.child, f.rr("unrelated.example. 60 IN A 192.0.2.2"))...)
			case "class":
				m.Answer[0].Header().Class = dns.ClassCHAOS
			}
			f.check(v, m, pipeline.ValidationBogus)
		})
	}
}

func TestAuthenticatedNegativeNSECAndAliases(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	base := f.sign(f.child, f.soa())
	apex := f.sign(f.child, f.rr("example. 300 IN NSEC z.example. NS SOA RRSIG NSEC DNSKEY"))
	nx := f.reply("missing.example.", dns.TypeA, dns.RcodeNameError, nil, append(base, apex...))
	f.check(v, nx, pipeline.ValidationSecure)
	nodata := f.reply("z.example.", dns.TypeAAAA, 0, nil, append(base, f.sign(f.child, f.rr("z.example. 300 IN NSEC example. A RRSIG NSEC"))...))
	f.check(v, nodata, pipeline.ValidationSecure)
	alias := f.reply("alias.example.", dns.TypeA, dns.RcodeNameError, f.sign(f.child, f.rr("alias.example. 60 IN CNAME missing.example.")), nx.Ns)
	f.check(v, alias, pipeline.ValidationSecure)
	for _, bad := range []string{"proof_missing", "type_exists", "name_exists", "signature"} {
		t.Run(bad, func(t *testing.T) {
			f.t = t
			m := nx.Copy()
			switch bad {
			case "proof_missing":
				m.Ns = base
			case "type_exists":
				m = nodata.Copy()
				m.Question[0].Qtype = dns.TypeA
			case "name_exists":
				m.Question[0].Name = "example."
			case "signature":
				m.Ns[2].(*dns.NSEC).NextDomain = "elsewhere."
			}
			f.check(v, m, pipeline.ValidationBogus)
		})
	}
}

func TestWildcardProofsAndDNAME(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	wild := f.sign(f.child, f.rr("*.example. 60 IN A 192.0.2.1"))
	wild[0].Header().Name = "www.example."
	wild[1].Header().Name = "www.example."
	nsec := f.sign(f.child, f.rr("*.example. 300 IN NSEC z.example. A RRSIG NSEC"))
	m := f.reply("www.example.", dns.TypeA, 0, wild, nsec)
	f.check(v, m, pipeline.ValidationSecure)
	m.Ns = nil
	f.check(v, m, pipeline.ValidationBogus)
	// NODATA at a wildcard needs next-closer absence plus an exact wildcard
	// bitmap proving the queried type and CNAME are absent.
	neg := f.reply("www.example.", dns.TypeAAAA, 0, nil, append(f.sign(f.child, f.soa()), nsec...))
	f.check(v, neg, pipeline.ValidationSecure)
	dname := f.sign(f.child, f.rr("alias.example. 60 IN DNAME target.example."))
	derived := f.rr("www.alias.example. 60 IN CNAME www.target.example.")
	m = f.reply("www.alias.example.", dns.TypeA, 0, append(append(dname, derived), f.positive("www.target.example.").Answer...), nil)
	f.check(v, m, pipeline.ValidationSecure)
	m.Answer[len(dname)].(*dns.CNAME).Target = "other.example."
	f.check(v, m, pipeline.ValidationBogus)
	// A wildcard-owner NSEC may be returned literally, but it cannot be
	// expanded into an exact-owner denial just by changing owner names.
	expanded := f.sign(f.child, f.rr("*.example. 300 IN NSEC z.example. A RRSIG NSEC"))
	for _, rr := range expanded {
		rr.Header().Name = "invented.example."
	}
	f.check(v, f.reply("invented.example.", dns.TypeAAAA, 0, nil, append(f.sign(f.child, f.soa()), expanded...)), pipeline.ValidationBogus)
}

func TestNSEC3NegativeWildcardAndWorkLimits(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	hash := dns.HashName("example.", 1, 0, "")
	n3 := f.rr(hash + ".example. 300 IN NSEC3 1 0 0 - " + hash + " NS SOA RRSIG DNSKEY")
	proof := f.sign(f.child, n3)
	m := f.reply("missing.example.", dns.TypeA, dns.RcodeNameError, nil, append(f.sign(f.child, f.soa()), proof...))
	f.check(v, m, pipeline.ValidationSecure)
	wild := f.sign(f.child, f.rr("*.example. 60 IN A 192.0.2.1"))
	for _, rr := range wild {
		rr.Header().Name = "www.example."
	}
	f.check(v, f.reply("www.example.", dns.TypeA, 0, wild, proof), pipeline.ValidationSecure)
	bad := m.Copy()
	bad.Ns[2].(*dns.NSEC3).Iterations = 151
	bad.Ns = append(bad.Ns[:2], f.sign(f.child, bad.Ns[2])...)
	f.check(v, bad, pipeline.ValidationIndeterminate)
	f.check(f.validator(Options{Limits: Limits{HashWork: 1}}), m, pipeline.ValidationIndeterminate)
	f.check(f.validator(Options{Limits: Limits{Queries: 1}}), f.positive("www.example."), pipeline.ValidationIndeterminate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := v.Validate(ctx, m.Question[0], m, f.lookup)
	if err == nil || r.Security.State != pipeline.ValidationIndeterminate {
		t.Fatal("canceled validation succeeded")
	}
}

func TestNSEC3PositiveWildcardWithoutClosestEncloserRecord(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	wild := f.sign(f.child, f.rr("*.wild.example. 60 IN A 192.0.2.1"))
	for _, rr := range wild {
		rr.Header().Name = "child.wild.example."
	}
	// A positive answer's authenticated RRSIG labels establish wild.example.
	// as its closest encloser. BIND sends only next-closer coverage here,
	// without an exact NSEC3 for that encloser (RFC 5155 section 8.8).
	hash := dns.HashName("other.example.", 1, 0, "")
	n3 := f.rr(hash + ".example. 300 IN NSEC3 1 0 0 - " + hash + " A RRSIG")
	m := f.reply("child.wild.example.", dns.TypeA, 0, wild, f.sign(f.child, n3))
	f.check(v, m, pipeline.ValidationSecure)
	for _, bad := range []string{"missing", "optout", "not_covered", "signature"} {
		t.Run(bad, func(t *testing.T) {
			f.t = t
			answer := m.Copy()
			switch bad {
			case "missing":
				answer.Ns = nil
			case "optout":
				answer.Ns[0].(*dns.NSEC3).Flags = 1
				answer.Ns = f.sign(f.child, answer.Ns[0])
			case "not_covered":
				h := dns.HashName("child.wild.example.", 1, 0, "")
				answer.Ns[0].Header().Name = h + ".example."
				answer.Ns[0].(*dns.NSEC3).NextDomain = h
				answer.Ns = f.sign(f.child, answer.Ns[0])
			case "signature":
				answer.Ns[1].(*dns.RRSIG).Signature = "AA=="
			}
			f.check(v, answer, pipeline.ValidationBogus)
		})
	}
}

func TestUnsignedDelegationRequiresAuthenticatedDSAbsence(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	plainSOA := f.rr("plain.example. 300 IN SOA ns.plain.example. hostmaster.plain.example. 1 3600 600 86400 300")
	for _, name := range []string{"www.plain.example.", "plain.example."} {
		f.replies[rrkey{name, dns.TypeSOA}] = f.reply(name, dns.TypeSOA, 0, nil, []dns.RR{plainSOA})
	}
	denial := append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("plain.example. 300 IN NSEC z.example. NS RRSIG NSEC"))...)
	f.replies[rrkey{"plain.example.", dns.TypeDS}] = f.reply("plain.example.", dns.TypeDS, 0, nil, denial)
	m := f.reply("www.plain.example.", dns.TypeA, 0, []dns.RR{f.rr("www.plain.example. 60 IN A 192.0.2.1")}, nil)
	r := f.check(v, m, pipeline.ValidationInsecure)
	if r.Security.ValidUntil.IsZero() {
		t.Fatal("DS absence dependency deadline lost")
	}
	f.check(v, f.reply("www.plain.example.", dns.TypeAAAA, 0, nil, []dns.RR{plainSOA}), pipeline.ValidationInsecure)
	// No NS delegation bit: ordinary NODATA for DS must not create an
	// insecure child zone and make forged unsigned answers acceptable.
	f.replies[rrkey{"plain.example.", dns.TypeDS}].Ns = append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("plain.example. 300 IN NSEC z.example. A RRSIG NSEC"))...)
	f.check(v, m, pipeline.ValidationBogus)
}

func TestNSEC3OptOutInsecureDelegation(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	soa := f.rr("plain.example. 300 IN SOA ns.plain.example. hostmaster.plain.example. 1 3600 600 86400 300")
	f.replies[rrkey{"www.plain.example.", dns.TypeSOA}] = f.reply("www.plain.example.", dns.TypeSOA, 0, nil, []dns.RR{soa})
	h := dns.HashName("example.", 1, 0, "")
	n3 := f.rr(h + ".example. 300 IN NSEC3 1 1 0 - " + h + " NS SOA RRSIG DNSKEY")
	f.replies[rrkey{"plain.example.", dns.TypeDS}] = f.reply("plain.example.", dns.TypeDS, 0, nil, append(f.sign(f.child, f.soa()), f.sign(f.child, n3)...))
	m := f.reply("www.plain.example.", dns.TypeA, 0, []dns.RR{f.rr("www.plain.example. 60 IN A 192.0.2.1")}, nil)
	f.check(v, m, pipeline.ValidationInsecure)
	n3.(*dns.NSEC3).Flags = 0
	f.replies[rrkey{"plain.example.", dns.TypeDS}].Ns = append(f.sign(f.child, f.soa()), f.sign(f.child, n3)...)
	f.check(v, m, pipeline.ValidationBogus)
}

func TestDirectDSOptOutAndParentAuthority(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	h := dns.HashName("example.", 1, 0, "")
	n3 := f.rr(h + ".example. 300 IN NSEC3 1 1 0 - " + h + " NS SOA RRSIG DNSKEY")
	m := f.reply("plain.example.", dns.TypeDS, 0, nil, append(f.sign(f.child, f.soa()), f.sign(f.child, n3)...))
	f.check(v, m, pipeline.ValidationInsecure)
	bad := m.Copy()
	bad.Rcode = dns.RcodeNameError
	f.check(v, bad, pipeline.ValidationBogus)
	n3.(*dns.NSEC3).Flags = 0
	m.Ns = append(f.sign(f.child, f.soa()), f.sign(f.child, n3)...)
	f.check(v, m, pipeline.ValidationBogus)
	// Exact signed NODATA for a non-delegation name remains secure.
	m.Ns = append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("plain.example. 300 IN NSEC z.example. A RRSIG NSEC"))...)
	f.check(v, m, pipeline.ValidationSecure)
	// The child zone itself cannot deny the DS that is held by its parent.
	m = f.reply("example.", dns.TypeDS, 0, nil, append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("example. 300 IN NSEC z.example. SOA NS RRSIG NSEC DNSKEY"))...))
	f.check(v, m, pipeline.ValidationBogus)
}

func TestNTAExpiryIdentityAndImmutability(t *testing.T) {
	f := newFixture(t)
	nta := NTA{Domain: "EXAMPLE", Reason: "operator recovery", ExpiresAt: f.now.Add(time.Hour)}
	anchors := []dns.RR{f.root.key.ToDS(dns.SHA256)}
	ntas := []NTA{nta}
	v := f.validator(Options{Anchors: anchors, NTAs: ntas})
	id := v.ActiveContextID()
	anchors[0].(*dns.DS).Digest = strings.Repeat("00", 32)
	ntas[0].Domain = "other."
	m := f.positive("www.example.")
	m.Answer = m.Answer[:1]
	r := f.check(v, m, pipeline.ValidationInsecure)
	if f.calls != 0 || !r.Security.ValidUntil.Equal(nta.ExpiresAt) {
		t.Fatal("NTA does not enforce absolute deadline")
	}
	f.now = nta.ExpiresAt
	if v.ActiveContextID() == id || r.Security.AvailableAt(f.now) {
		t.Fatal("exact NTA expiry not enforced")
	}
	f.replies[rrkey{"www.example.", dns.TypeSOA}] = f.reply("www.example.", dns.TypeSOA, 0, nil, f.sign(f.child, f.soa()))
	f.check(v, m, pipeline.ValidationBogus)
	if _, err := New(Options{NTAs: []NTA{{Domain: "example.", Reason: "too long", ExpiresAt: f.now.Add(7*24*time.Hour + time.Second)}}, Now: func() time.Time { return f.now }}); err == nil {
		t.Fatal("accepted NTA beyond seven days")
	}
}

func TestIANAAnchorsAndCanonicalNSECOrder(t *testing.T) {
	a := RootAnchors()
	if len(a) != 2 || a[0].(*dns.DS).KeyTag != 20326 || a[1].(*dns.DS).KeyTag != 38696 {
		t.Fatal("current root DS set incorrect")
	}
	a[0].(*dns.DS).Digest = "changed"
	if RootAnchors()[0].(*dns.DS).Digest == "changed" {
		t.Fatal("anchor alias")
	}
	if compareNames("z.a.", "a.z.") >= 0 || compareNames("a\\046b.example.", "a.b.example.") == 0 || compareNames("EXAMPLE.", "example.") != 0 {
		t.Fatal("DNS canonical name order incorrect")
	}
}
