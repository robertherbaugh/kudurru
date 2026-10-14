package dnssec

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestAnchorMaterialValidation(t *testing.T) {
	var nilKey *dns.DNSKEY
	if _, err := New(Options{Anchors: []dns.RR{nilKey}}); err == nil {
		t.Fatal("nil anchor accepted")
	}
	for _, alg := range []uint8{dns.RSASHA256, dns.RSASHA512, dns.ECDSAP256SHA256, dns.ECDSAP384SHA384, dns.ED25519} {
		t.Run(dns.AlgorithmToString[alg], func(t *testing.T) {
			k := &dns.DNSKEY{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET}, Flags: 257, Protocol: 3, Algorithm: alg}
			bits := 256
			if alg == dns.RSASHA256 || alg == dns.RSASHA512 {
				bits = 1024
			}
			if alg == dns.ECDSAP384SHA384 {
				bits = 384
			}
			if _, err := k.Generate(bits); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Options{Anchors: []dns.RR{k}}); err != nil {
				t.Fatalf("valid material rejected: %v", err)
			}
			for _, bad := range []string{"", "%%%", k.PublicKey + "\n", base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), base64.StdEncoding.EncodeToString(make([]byte, 64)), base64.StdEncoding.EncodeToString(make([]byte, 32))} {
				badKey := dns.Copy(k).(*dns.DNSKEY)
				badKey.PublicKey = bad
				if _, err := New(Options{Anchors: []dns.RR{badKey}}); err == nil {
					t.Fatalf("invalid key material accepted for algorithm %d", alg)
				}
			}
		})
	}
}

func TestNTAInputAndExpiredConfiguration(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, domain := range []string{"", " ", "\t", "\v", "\f", "\u00a0", " example.", "example.\n", "ex ample.", "ex\u2003ample."} {
		if _, err := New(Options{NTAs: []NTA{{Domain: domain, Reason: "test", ExpiresAt: now.Add(time.Hour)}}, Now: func() time.Time { return now }}); err == nil {
			t.Fatalf("NTA domain %q accepted", domain)
		}
	}
	if _, err := New(Options{NTAs: []NTA{{Domain: "EXAMPLE", Reason: "test", ExpiresAt: now.Add(time.Hour)}, {Domain: "example.", Reason: "test", ExpiresAt: now.Add(time.Hour)}}, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("duplicate canonical NTA accepted")
	}
	v, err := New(Options{NTAs: []NTA{{Domain: "example.", Reason: "expired retained config", ExpiresAt: now.Add(-time.Hour)}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.exception("www.example.", now); ok {
		t.Fatal("expired NTA activated")
	}
}

func TestClosestOperatorAnchorCannotBeBypassed(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{Anchors: []dns.RR{f.root.key.ToDS(dns.SHA256), f.child.key.ToDS(dns.SHA256)}})
	// A parent key cannot sign ordinary child-zone data past an explicitly
	// configured trust boundary, even though that parent's key is trusted.
	m := f.reply("www.example.", dns.TypeA, 0, f.sign(f.root, f.rr("www.example. 60 IN A 192.0.2.1")), nil)
	f.check(v, m, pipeline.ValidationBogus)
}

func TestUnsignedApexDiscoveryCannotBypassCloserAnchor(t *testing.T) {
	f := newFixture(t)
	local := dns.Copy(f.child.key).(*dns.DNSKEY)
	local.Hdr.Name = "secure.plain.example."
	v := f.validator(Options{Anchors: []dns.RR{f.root.key.ToDS(dns.SHA256), local}})
	// The untrusted SOA discovery claims the unsigned parent zone, but a
	// configured child anchor still requires authentication below its apex.
	f.replies[rrkey{"www.secure.plain.example.", dns.TypeSOA}] = f.reply("www.secure.plain.example.", dns.TypeSOA, 0, nil, []dns.RR{f.rr("plain.example. 300 IN SOA ns.plain.example. hostmaster.plain.example. 1 3600 600 86400 300")})
	f.replies[rrkey{"plain.example.", dns.TypeDS}] = f.reply("plain.example.", dns.TypeDS, 0, nil, append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("plain.example. 300 IN NSEC z.example. NS RRSIG NSEC"))...))
	m := f.reply("www.secure.plain.example.", dns.TypeA, 0, []dns.RR{f.rr("www.secure.plain.example. 60 IN A 192.0.2.1")}, nil)
	r := f.check(v, m, pipeline.ValidationBogus)
	if r.Security.Reason != "zone_above_anchor" {
		t.Fatalf("anchor boundary lost: %+v", r.Security)
	}
}

func TestParentDSDenialCannotBypassCloserAnchor(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{Anchors: []dns.RR{f.root.key.ToDS(dns.SHA256), f.child.key}})
	// Even an authenticated ancestor must not supply an insecure delegation
	// past a locally configured trust boundary.
	f.replies[rrkey{"plain.example.", dns.TypeDS}] = f.reply("plain.example.", dns.TypeDS, 0, nil, append(f.sign(f.root, f.rr(". 300 IN SOA ns. hostmaster. 1 3600 600 86400 300")), f.sign(f.root, f.rr("plain.example. 300 IN NSEC z.example. NS RRSIG NSEC"))...))
	soa := f.rr("plain.example. 300 IN SOA ns.plain.example. hostmaster.plain.example. 1 3600 600 86400 300")
	f.replies[rrkey{"www.plain.example.", dns.TypeSOA}] = f.reply("www.plain.example.", dns.TypeSOA, 0, nil, []dns.RR{soa})
	r := f.check(v, f.reply("www.plain.example.", dns.TypeA, 0, []dns.RR{f.rr("www.plain.example. 60 IN A 192.0.2.1")}, nil), pipeline.ValidationBogus)
	if r.Security.Reason != "parent_above_anchor" {
		t.Fatalf("parent crossed anchor: %+v", r.Security)
	}
}

func TestEmptyNonterminalAndDelegationNODATA(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	ent := f.reply("branch.example.", dns.TypeA, 0, nil, append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("aaa.example. 300 IN NSEC child.branch.example. A RRSIG NSEC"))...))
	f.check(v, ent, pipeline.ValidationSecure)
	cut := f.reply("plain.example.", dns.TypeA, 0, nil, append(f.sign(f.child, f.soa()), f.sign(f.child, f.rr("plain.example. 300 IN NSEC z.example. NS RRSIG NSEC"))...))
	f.check(v, cut, pipeline.ValidationBogus)
}

func TestUnsupportedOnlyAuthenticatedDSIsInsecure(t *testing.T) {
	f := newFixture(t)
	v := f.validator(Options{})
	ds := f.child.key.ToDS(dns.SHA256)
	ds.Algorithm = 250
	ds.Digest = strings.Repeat("00", 32)
	f.replies[rrkey{"example.", dns.TypeDS}].Answer = f.sign(f.root, ds)
	f.check(v, f.positive("www.example."), pipeline.ValidationInsecure)
}
