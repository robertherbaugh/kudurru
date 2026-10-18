package zone

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestContentHashIsCanonical(t *testing.T) {
	base := exampleEndpoints()
	h := exampleZone(t).ContentHash()

	// Endpoint order, name case, target order and trailing dots do not
	// matter.
	shuffled := slices.Clone(base)
	slices.Reverse(shuffled)
	shuffled = append(shuffled[:0:0], shuffled...)
	for i := range shuffled {
		e := shuffled[i]
		e.Targets = slices.Clone(e.Targets)
		slices.Reverse(e.Targets)
		shuffled[i] = e
	}
	if got := mustBuild(t, dnsZone("example.org."), shuffled...).ContentHash(); got != h {
		t.Fatalf("hash depends on input order: %s vs %s", got, h)
	}

	// The serial is not content.
	other := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Status.Serial = 7 }), base...)
	if other.ContentHash() != h {
		t.Fatal("serial changed the hash")
	}

	// Real changes do.
	for name, z := range map[string]*Zone{
		"ttl":    mustBuild(t, dnsZone("example.org."), append(slices.Clone(base), epTTL("new.example.org", "A", 30, "192.0.2.1"))...),
		"record": mustBuild(t, dnsZone("example.org."), append(slices.Clone(base), ep("new.example.org", "A", "192.0.2.1"))...),
		"soa":    mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.SOA.Refresh = 1 }), base...),
		"ns":     mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Nameservers = dz.Spec.Nameservers[:1] }), base...),
	} {
		if z.ContentHash() == h {
			t.Errorf("%s change did not change the hash", name)
		}
	}
	// Disabled and expired records hash like absent ones; a live record
	// with a future expiry counts. base ends with disabled, expired,
	// expiring and view-scoped endpoints.
	n := len(base)
	without := mustBuild(t, dnsZone("example.org."), base[:n-4]...).ContentHash()
	if mustBuild(t, dnsZone("example.org."), base[:n-2]...).ContentHash() != without {
		t.Fatal("disabled and expired records must hash like absent ones")
	}
	if mustBuild(t, dnsZone("example.org."), base[:n-1]...).ContentHash() == without {
		t.Fatal("the live expiring record must count")
	}
	if h != mustBuild(t, dnsZone("example.org."), base[:n-1]...).ContentHash() {
		t.Fatal("a view-scoped record is not served and must not count")
	}
}

func TestCanonicalRdataLowercasesNames(t *testing.T) {
	a := canonicalRdata(rr(t, "x. 1 IN CNAME WWW.Example.ORG."))
	b := canonicalRdata(rr(t, "x. 1 IN CNAME www.example.org."))
	if !slices.Equal(a, b) {
		t.Fatal("CNAME targets compare case-insensitively")
	}
	a = canonicalRdata(rr(t, `x. 1 IN TXT "ABC"`))
	b = canonicalRdata(rr(t, `x. 1 IN TXT "abc"`))
	if slices.Equal(a, b) {
		t.Fatal("TXT is case-sensitive")
	}
}

func TestNextSerial(t *testing.T) {
	now := time.Unix(1791720000, 0)
	for _, tc := range []struct {
		prev uint32
		now  time.Time
		want uint32
	}{
		{0, now, 1791720000},
		{1791720000, now, 1791720001},     // same second: +1
		{2026101101, now, 2026101102},     // YYYYMMDDnn serial ahead of the clock
		{100, time.Unix(-5, 0), 101},      // pre-1970 clock counts as 0
		{math.MaxUint32, now, 1791720000}, // wrap: prev+1 is 0
		{5, time.Unix(1<<32+10, 0), 10},   // after 2106 the clock wraps
		{1791719999, now.Add(time.Hour), 1791723600},
	} {
		if got := NextSerial(tc.prev, tc.now); got != tc.want {
			t.Errorf("NextSerial(%d, %d) = %d, want %d", tc.prev, tc.now.Unix(), got, tc.want)
		}
	}
}

func TestStampSerial(t *testing.T) {
	now := time.Unix(1791720000, 0)
	if s, changed := StampSerial(1791700000, "sha256:a", "sha256:a", now); changed || s != 1791700000 {
		t.Fatalf("same hash keeps the serial: %d %v", s, changed)
	}
	if s, changed := StampSerial(1791700000, "sha256:a", "sha256:b", now); !changed || s != 1791720000 {
		t.Fatalf("new hash bumps: %d %v", s, changed)
	}
	if s, changed := StampSerial(0, "", "", now); !changed || s != 1791720000 {
		t.Fatalf("an unstamped zone gets a serial: %d %v", s, changed)
	}
}

func TestDiff(t *testing.T) {
	base := mustBuild(t, dnsZone("example.org."),
		ep("a.example.org", "A", "192.0.2.1", "192.0.2.2"),
		ep("b.example.org", "TXT", "keep"),
	)
	next := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Status.Serial++ }),
		ep("a.example.org", "A", "192.0.2.1", "192.0.2.3"),
		ep("b.example.org", "TXT", "keep"),
		ep("c.example.org", "AAAA", "2001:db8::1"),
	)
	d := Diff(base, next)
	if d.FromSerial != 2026101100 || d.ToSerial != 2026101101 || d.Zone != "example.org." {
		t.Fatalf("serials: %+v", d)
	}
	checkSection(t, "removed", d.Removed, []string{"a.example.org. 300 IN A 192.0.2.2"})
	checkSection(t, "added", d.Added, []string{"a.example.org. 300 IN A 192.0.2.3", "c.example.org. 300 IN AAAA 2001:db8::1"})
	if d.Empty() {
		t.Fatal("not empty")
	}
	if same := Diff(base, base); !same.Empty() {
		t.Fatalf("self diff: %+v", same)
	}
	full := Diff(nil, base)
	if len(full.Added) != base.RecordCount() || full.FromSerial != 0 {
		t.Fatalf("diff from nothing adds every record except the SOA: %d vs %d", len(full.Added), base.RecordCount())
	}
	for _, r := range full.Added {
		if r.Header().Rrtype == dns.TypeSOA {
			t.Fatal("SOA in delta")
		}
	}
}
