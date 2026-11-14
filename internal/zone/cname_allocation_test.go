package zone

import (
	"testing"

	"github.com/miekg/dns"
)

func TestLookupCNAMEAnswerAllocation(t *testing.T) {
	z := mustBuild(t, dnsZone("example.org."),
		ep("alias.example.org", "CNAME", "target.example.org"),
		ep("target.example.org", "A", "192.0.2.1"))
	var result Result
	allocations := testing.AllocsPerRun(1000, func() {
		result = z.Lookup("alias.example.org.", dns.TypeA)
	})
	if result.Kind != Success || result.Hops != 1 || len(result.Answer) != 2 ||
		result.Answer[0].Header().Rrtype != dns.TypeCNAME || result.Answer[1].Header().Rrtype != dns.TypeA {
		t.Fatalf("alias and target answer changed: %+v", result)
	}
	if allocations > 1 {
		t.Fatalf("alias and target require %g allocations, want at most one", allocations)
	}
	// Target ownership is still checked before following the alias. The extra
	// capacity must not expose the target or bypass the cross-zone handoff.
	called := 0
	result = z.LookupWithin("alias.example.org.", dns.TypeA, 0, func(target string) bool {
		called++
		if target != "target.example.org." {
			t.Fatalf("unexpected target %q", target)
		}
		return false
	})
	if called != 1 || result.Kind != CNAME || result.Hops != 1 || len(result.Answer) != 1 || result.Target != "target.example.org." {
		t.Fatalf("target ownership handoff changed: %+v, calls %d", result, called)
	}
}
