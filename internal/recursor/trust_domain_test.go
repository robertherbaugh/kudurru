package recursor

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

func TestTrustDomainRetainsNativeExceptionsAndResolverIncarnation(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	cfg := DefaultConfig()
	cfg.IPv6 = IPv6Off
	cfg.NegativeTrustAnchors = []dnssec.NTA{{Domain: "broken.example.", Reason: "fixture", ExpiresAt: expires}}
	makeResolver := func(domain [32]byte) *Resolver {
		r, err := New(cfg, Options{Now: func() time.Time { return now }, TrustDomain: domain})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	static, a, b := makeResolver([32]byte{}), makeResolver([32]byte{1}), makeResolver([32]byte{2})
	zero, until := static.TrustContext()
	oldA, untilA := a.TrustContext()
	oldB, untilB := b.TrustContext()
	if zero == oldA || oldA == oldB || !until.Equal(expires) || untilA != until || untilB != until {
		t.Fatal("native domain omitted or original NTA deadline changed")
	}
	if err := a.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	if current, _ := a.TrustContext(); current != oldA {
		t.Fatal("same resolver configuration renewed its input domain")
	}
	now = expires
	newA, nextA := a.TrustContext()
	newB, nextB := b.TrustContext()
	if newA == oldA || newB == oldB || newA == newB || !nextA.IsZero() || !nextB.IsZero() {
		t.Fatal("expired native exception or incarnation collapsed")
	}
	// Existing exception identity builds its active set on every observation.
	// Adding the fixed managed domain must not add allocations to that path.
	baseline := testing.AllocsPerRun(100, func() { static.TrustContext() })
	if allocs := testing.AllocsPerRun(100, func() { a.TrustContext() }); allocs != baseline {
		t.Fatal("native domain adds query identity allocations", baseline, allocs)
	}
}
