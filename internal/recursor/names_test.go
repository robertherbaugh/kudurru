package recursor

import (
	"sort"
	"testing"

	"github.com/miekg/dns"
)

func TestCanonicalOrder(t *testing.T) {
	// RFC 4034 §6.1 example.
	want := []string{
		"example.", "a.example.", "yljkjljk.a.example.", "Z.a.example.",
		"zABC.a.EXAMPLE.", "z.example.", "\\001.z.example.", "*.z.example.", "\\200.z.example.",
	}
	got := append([]string(nil), want...)
	sort.Slice(got, func(i, j int) bool { return canonicalCompare(got[len(got)-1-i], got[len(got)-1-j]) > 0 })
	sort.Slice(got, func(i, j int) bool { return canonicalCompare(got[i], got[j]) < 0 })
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestNameHelpers(t *testing.T) {
	if parentName("www.example.com.") != "example.com." || parentName("com.") != "." || parentName(".") != "." {
		t.Fatal("parentName")
	}
	if childToward(".", "www.example.com.") != "com." || childToward("example.com.", "a.b.example.com.") != "b.example.com." {
		t.Fatal("childToward")
	}
	if commonAncestor("a.b.example.com.", "c.example.com.") != "example.com." || commonAncestor("a.com.", "b.org.") != "." {
		t.Fatal("commonAncestor")
	}
	if nextMinimized(".", "a.b.c.d.e.f.g.h.i.j.k.l.example.com.", 0) != "com." {
		t.Fatal("first step adds one label")
	}
	// After four one-label steps the rest is split so the name takes at
	// most maxMinimiseStep queries.
	n, known := 0, "."
	name := "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa."
	for known != name {
		known = nextMinimized(known, name, n)
		n++
	}
	if n > maxMinimiseStep+1 {
		t.Fatalf("%d steps", n)
	}
	r := randomizeCase("www.example.com.", func() uint64 { return 0xAAAAAAAAAAAAAAAA })
	if !equalName(r, "www.example.com.") || r == "www.example.com." {
		t.Fatalf("randomizeCase %q", r)
	}
	if labels(".") != 0 || labels("a.b.") != 2 || suffixLabels("a.b.c.", 2) != "b.c." {
		t.Fatal("labels")
	}
	_ = dns.Fqdn
}

func TestNSECCover(t *testing.T) {
	n := &dns.NSEC{Hdr: dns.RR_Header{Name: "b.example."}, NextDomain: "d.example."}
	for name, want := range map[string]bool{"c.example.": true, "x.b.example.": true, "b.example.": false, "d.example.": false, "a.example.": false, "e.example.": false} {
		if nsecCovers(n, name) != want {
			t.Errorf("cover %s: %v", name, !want)
		}
	}
	last := &dns.NSEC{Hdr: dns.RR_Header{Name: "y.example."}, NextDomain: "example."}
	if !nsecCovers(last, "z.example.") || nsecCovers(last, "a.example.") || nsecCovers(last, "z.other.") {
		t.Fatal("last NSEC wraps to the apex only")
	}
	deleg := &dns.NSEC{Hdr: dns.RR_Header{Name: "sub.example."}, NextDomain: "z.example.", TypeBitMap: []uint16{dns.TypeNS, dns.TypeRRSIG, dns.TypeNSEC}}
	if nsecCover("x.sub.example.", []*dns.NSEC{deleg}) != nil {
		t.Fatal("a delegation NSEC cannot deny names below the cut")
	}
}
