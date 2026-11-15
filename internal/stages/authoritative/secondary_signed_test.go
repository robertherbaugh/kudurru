package authoritative

import (
	"testing"

	"github.com/miekg/dns"
)

func TestMinimalSignedANYRetainsOnlyChosenRRsetAndItsSignatures(t *testing.T) {
	answer := []dns.RR{mustRR("example.org. 300 IN NS ns.example.org."), mustRR("example.org. 300 IN RRSIG NS 13 2 300 20270101000000 20260101000000 1234 example.org. AQID"), mustRR("example.org. 300 IN SOA ns.example.org. admin.example.org. 1 10 3 3600 60"), mustRR("example.org. 300 IN RRSIG SOA 13 2 300 20270101000000 20260101000000 1234 example.org. AQID")}
	out := minimalSignedANY(answer)
	if len(out) != 2 || out[0].Header().Rrtype != dns.TypeNS || out[1].(*dns.RRSIG).TypeCovered != dns.TypeNS {
		t.Fatalf("UDP ANY signature pairing lost: %v", out)
	}
}

func TestSignedAliasAuthorityDeduplicatesAndPreservesNegativeProofs(t *testing.T) {
	proof := mustRR("alias.example.org. 60 IN NSEC next.example.org. CNAME RRSIG NSEC")
	soa := mustRR("example.org. 60 IN SOA ns.example.org. admin.example.org. 1 10 3 3600 60")
	merged := appendSignedAuthority([]dns.RR{proof}, []dns.RR{soa, dns.Copy(proof)})
	if len(merged) != 2 || merged[1].Header().Rrtype != dns.TypeSOA {
		t.Fatal("wildcard/negative alias evidence lost or duplicated")
	}
}
