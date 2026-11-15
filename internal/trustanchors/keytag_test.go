package trustanchors

import (
	"testing"

	"github.com/miekg/dns"
)

func TestKeyTagIsTheIdentityTagAcrossRevocation(t *testing.T) {
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	if _, err := key.Generate(256); err != nil {
		t.Fatal(err)
	}
	want := key.KeyTag()
	revoked := *key
	revoked.Flags |= dns.REVOKE
	if revoked.KeyTag() == want {
		t.Fatal("fixture: the revoked tag should differ")
	}
	for _, rr := range []*dns.DNSKEY{key, &revoked} {
		tag, algorithm, err := Key{DNSKEY: rr.String()}.KeyTag()
		if err != nil || tag != want || algorithm != dns.ED25519 {
			t.Fatal("key tag is not the identity tag", tag, want, algorithm, err)
		}
	}
	if _, _, err := (Key{DNSKEY: ". 0 IN A 192.0.2.1"}).KeyTag(); err == nil {
		t.Fatal("a non-DNSKEY record produced a key tag")
	}
	if _, _, err := (Key{}).KeyTag(); err == nil {
		t.Fatal("an empty key produced a key tag")
	}
}
