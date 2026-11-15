package zonestate

import (
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// ownershipView returns a committed view whose aggregate holds the dynamic
// records (no source) and the static records (source "s") given.
func ownershipView(t testing.TB, dynamic, static []string) *View {
	t.Helper()
	a := &Aggregate{}
	add := func(text, source string) {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := FromRR(rr, Provenance{SourceID: source})
		if err != nil {
			t.Fatal(err)
		}
		a.Records = append(a.Records, rec)
	}
	for _, text := range dynamic {
		add(text, "")
	}
	for _, text := range static {
		add(text, "s")
	}
	return &View{Head: &zonekinds.ZoneState{}, Aggregate: a}
}

// bruteConflicts is the rule Conflicts indexes: rr conflicts with a dynamic
// record when projectionConflicts says so for any of them.
func bruteConflicts(dynamic []dns.RR, rr dns.RR) bool {
	name, err := dynamicupdate.CanonicalName(rr.Header().Name)
	if err != nil {
		return false
	}
	r := []dynamicupdate.Reservation{{Name: name, Type: rr.Header().Rrtype, Subtree: rr.Header().Rrtype == dns.TypeDNAME}}
	for _, d := range dynamic {
		if projectionConflicts(d, r) {
			return true
		}
	}
	return false
}

// TestDynamicOwnershipConflictsMatchesTheScan proves the name index answers
// exactly what a scan of every dynamic record answers (D373), for records
// at, above and below dynamic names and DNAMEs, mixed case and escapes.
func TestDynamicOwnershipConflictsMatchesTheScan(t *testing.T) {
	dynamicText := []string{
		"dyn.example. 300 IN A 192.0.2.1",
		"Deep.Below.Sub.example. 300 IN TXT \"x\"",
		"moved.example. 300 IN DNAME elsewhere.test.",
		"alias.example. 300 IN CNAME target.test.",
		"a\\.b.example. 300 IN A 192.0.2.2",
	}
	var dynamic []dns.RR
	for _, text := range dynamicText {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		dynamic = append(dynamic, rr)
	}
	o, err := NewDynamicOwnership(ownershipView(t, dynamicText, []string{"static.example. 300 IN A 192.0.2.9"}))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"example.", "dyn.example.", "DYN.example.", "x.dyn.example.", "sub.example.", "below.sub.example.", "deep.below.sub.example.",
		"other.deep.below.sub.example.", "moved.example.", "x.moved.example.", "y.x.moved.example.", "alias.example.", "a\\.b.example.", "b.example.", "static.example.", "."}
	types := []string{"A 192.0.2.7", "TXT \"y\"", "CNAME t.test.", "DNAME t.test.", "NS ns.test."}
	want := map[string]bool{
		// Same name and type, or a CNAME on either side.
		"dyn.example. A": true, "dyn.example. CNAME": true, "DYN.example. A": true, "alias.example. A": true, "alias.example. TXT": true,
		// Below a dynamic DNAME, and the DNAME's own name.
		"moved.example. A": true, "x.moved.example. TXT": true, "y.x.moved.example. NS": true,
		// A static DNAME at or above a dynamic record.
		"example. DNAME": true, "sub.example. DNAME": true, "deep.below.sub.example. DNAME": true, ". DNAME": true,
	}
	for _, name := range names {
		for _, typ := range types {
			rr, err := dns.NewRR(name + " 300 IN " + typ)
			if err != nil {
				t.Fatal(err)
			}
			got, brute := o.Conflicts(rr), bruteConflicts(dynamic, rr)
			if got != brute {
				t.Errorf("%s %s: index %v, scan %v", name, typ, got, brute)
			}
			key := name + " " + dns.TypeToString[rr.Header().Rrtype]
			if w, listed := want[key]; listed && got != w {
				t.Errorf("%s: conflict %v, want %v", key, got, w)
			}
		}
	}
	if empty, err := NewDynamicOwnership(&View{}); err != nil || !empty.Empty() || empty.Conflicts(dynamic[0]) {
		t.Fatalf("a view without a head: %v, empty %v", err, empty.Empty())
	}
}

// ownershipAtScale builds the shape whose cost the scan made quadratic:
// static records against many dynamic DNAMEs, and static DNAMEs against
// many dynamic records.
func ownershipAtScale(t testing.TB, dynamicNames, staticRecords int) (*DynamicOwnership, []dns.RR) {
	t.Helper()
	var dynamic []string
	for i := range dynamicNames {
		dynamic = append(dynamic, fmt.Sprintf("d%d.moved.example. 300 IN DNAME elsewhere.test.", i), fmt.Sprintf("r%d.dyn.example. 300 IN A 192.0.2.1", i))
	}
	o, err := NewDynamicOwnership(ownershipView(t, dynamic, nil))
	if err != nil {
		t.Fatal(err)
	}
	static := make([]dns.RR, 0, staticRecords)
	for i := range staticRecords {
		typ := "A 192.0.2.3"
		if i%10 == 0 {
			typ = "DNAME t.test."
		}
		rr, err := dns.NewRR(fmt.Sprintf("s%d.www.example. 300 IN %s", i, typ))
		if err != nil {
			t.Fatal(err)
		}
		static = append(static, rr)
	}
	return o, static
}

// TestDynamicOwnershipConflictsAtTheSourceLimit checks 100,000 static
// records against 20,000 dynamic DNAMEs and 20,000 other dynamic records.
// The scan cost about 2.4 billion comparisons for this shape; the index
// answers it in well under a second (the bound below is generous for a
// shared, race-instrumented machine and is not a performance target).
func TestDynamicOwnershipConflictsAtTheSourceLimit(t *testing.T) {
	o, static := ownershipAtScale(t, 20000, 100000)
	start := time.Now()
	for _, rr := range static {
		if o.Conflicts(rr) {
			t.Fatalf("%s conflicts", rr)
		}
	}
	elapsed := time.Since(start)
	t.Logf("timing: %d checks in %s", len(static), elapsed.Round(time.Millisecond))
	if elapsed > 30*time.Second {
		t.Fatalf("checks took %s", elapsed)
	}
	// The same shape still finds what it should.
	for _, text := range []string{"x.d7.moved.example. 300 IN A 192.0.2.4", "dyn.example. 300 IN DNAME t.test.", "r9.dyn.example. 300 IN CNAME t.test."} {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		if !o.Conflicts(rr) {
			t.Errorf("%s does not conflict", text)
		}
	}
}

func BenchmarkDynamicOwnershipConflicts(b *testing.B) {
	o, static := ownershipAtScale(b, 20000, 100000)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		o.Conflicts(static[i%len(static)])
	}
}
