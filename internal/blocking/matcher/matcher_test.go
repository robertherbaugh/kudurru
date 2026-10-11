package matcher

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"
)

func TestExact(t *testing.T) {
	var b ExactBuilder
	for _, n := range []string{"example.org", "ads.example.com", "a.b", "example.org", "x"} {
		if !b.Add(n, 1) {
			t.Fatalf("Add(%q) = false", n)
		}
	}
	b.Add("tagged.example", 7)
	if b.Add("", 1) || b.Add(strings.Repeat("a", MaxNameLen+1), 1) {
		t.Fatal("Add accepted an empty or over-long name")
	}
	if got := b.Added(); got != 6 {
		t.Fatalf("Added() = %d, want 6", got)
	}
	e := b.Build()
	if got := e.Len(); got != 5 {
		t.Fatalf("Len() = %d, want 5 (one duplicate)", got)
	}
	if b.Added() != 0 {
		t.Fatal("Build did not reset the builder")
	}
	tests := []struct {
		name string
		ok   bool
		tag  uint8
	}{
		{"example.org", true, 1},
		{"ads.example.com", true, 1},
		{"a.b", true, 1},
		{"x", true, 1},
		{"tagged.example", true, 7},
		{"www.example.org", false, 0},
		{"example.or", false, 0},
		{"example.orh", false, 0},
		{"", false, 0},
		{strings.Repeat("z", 300), false, 0},
	}
	for _, tc := range tests {
		tag, ok := e.Match(tc.name)
		if ok != tc.ok || tag != tc.tag {
			t.Errorf("Match(%q) = (%d, %v), want (%d, %v)", tc.name, tag, ok, tc.tag, tc.ok)
		}
		tag, ok = e.MatchHash(tc.name, Hash(tc.name))
		if ok != tc.ok || tag != tc.tag {
			t.Errorf("MatchHash(%q) = (%d, %v), want (%d, %v)", tc.name, tag, ok, tc.tag, tc.ok)
		}
	}
	if e.Bytes() <= 0 {
		t.Error("Bytes() <= 0")
	}
}

func TestNilMatchersAreEmpty(t *testing.T) {
	var e *Exact
	var s *Suffix
	var r *Regex
	var ip *IPSet
	if _, ok := e.Match("a"); ok {
		t.Error("nil Exact matched")
	}
	if _, _, ok := s.Match("a"); ok {
		t.Error("nil Suffix matched")
	}
	if _, _, ok := r.Match("a"); ok {
		t.Error("nil Regex matched")
	}
	if _, ok := ip.Match(netip.MustParseAddr("192.0.2.1")); ok {
		t.Error("nil IPSet matched")
	}
	if e.Len()+s.Len()+r.Len()+ip.Len() != 0 || e.Bytes()+s.Bytes()+r.Bytes()+ip.Bytes() != 0 {
		t.Error("nil matcher reports entries or bytes")
	}
	var eb ExactBuilder
	var sb SuffixBuilder
	var rb RegexBuilder
	var ib IPSetBuilder
	if eb.Build() != nil || sb.Build() != nil || rb.Build() != nil || ib.Build() != nil {
		t.Error("empty builder returned a non-nil matcher")
	}
}

func TestExactMixedTags(t *testing.T) {
	var b ExactBuilder
	b.Add("bbb.example", 2)
	b.Add("aaa.example", 1)
	b.Add("ccc.example", 3)
	b.Add("aaa.example", 9) // duplicate: the first tag wins
	e := b.Build()
	for name, want := range map[string]uint8{"aaa.example": 1, "bbb.example": 2, "ccc.example": 3} {
		if tag, ok := e.Match(name); !ok || tag != want {
			t.Errorf("Match(%q) = (%d, %v), want (%d, true)", name, tag, ok, want)
		}
	}
}

func TestSuffix(t *testing.T) {
	var b SuffixBuilder
	add := func(base string, tag uint8) {
		t.Helper()
		if !b.Add(base, tag) {
			t.Fatalf("Add(%q) = false", base)
		}
	}
	add("example.org", 1)
	add("ads.example.com", 2)
	add("deep.sub.tracker.net", 3)
	add("zip", 4) // a whole TLD
	add("example.org", 9)
	for _, bad := range []string{"", ".com", "com.", strings.Repeat("a", MaxNameLen+1)} {
		if b.Add(bad, 1) {
			t.Errorf("Add(%q) = true, want false", bad)
		}
	}
	s := b.Build()
	if got := s.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
	tests := []struct {
		name string
		base string
		tag  uint8
		ok   bool
	}{
		{"example.org", "example.org", 1, true},
		{"www.example.org", "example.org", 1, true},
		{"a.b.c.example.org", "example.org", 1, true},
		{"notexample.org", "", 0, false},
		{"example.org.evil", "", 0, false},
		{"org", "", 0, false},
		{"ads.example.com", "ads.example.com", 2, true},
		{"x.ads.example.com", "ads.example.com", 2, true},
		{"example.com", "", 0, false},
		{"bads.example.com", "", 0, false},
		{"tracker.net", "", 0, false},
		{"sub.tracker.net", "", 0, false},
		{"deep.sub.tracker.net", "deep.sub.tracker.net", 3, true},
		{"a.deep.sub.tracker.net", "deep.sub.tracker.net", 3, true},
		{"zip", "zip", 4, true},
		{"anything.zip", "zip", 4, true},
		{"zipper", "", 0, false},
		{"", "", 0, false},
		{".", "", 0, false},
		{"..org", "", 0, false},
		{"x..example.org", "example.org", 1, true},
	}
	for _, tc := range tests {
		base, tag, ok := s.Match(tc.name)
		if ok != tc.ok || base != tc.base || tag != tc.tag {
			t.Errorf("Match(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.name, base, tag, ok, tc.base, tc.tag, tc.ok)
		}
		base, tag, ok = s.MatchHash(tc.name, Hash(tc.name))
		if ok != tc.ok || base != tc.base || tag != tc.tag {
			t.Errorf("MatchHash(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.name, base, tag, ok, tc.base, tc.tag, tc.ok)
		}
	}
}

func TestSuffixBroadestFirst(t *testing.T) {
	var b SuffixBuilder
	b.Add("ads.example.com", 2)
	b.Add("example.com", 1)
	b.Add("x.ads.example.com", 3)
	s := b.Build()
	base, tag, ok := s.Match("y.x.ads.example.com")
	if !ok || base != "example.com" || tag != 1 {
		t.Fatalf("Match = (%q, %d, %v), want the broadest base example.com", base, tag, ok)
	}
}

// TestRandomAgainstReference cross-checks Exact and Suffix against map-based
// reference implementations on random names, including filter collisions.
func TestRandomAgainstReference(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test data
	tlds := []string{"com", "net", "org", "io", "de", "x"}
	label := func() string {
		const alphabet = "abcdefghij-0123"
		n := 1 + r.IntN(6)
		var sb strings.Builder
		for range n {
			sb.WriteByte(alphabet[r.IntN(len(alphabet))])
		}
		return sb.String()
	}
	name := func() string {
		parts := []string{tlds[r.IntN(len(tlds))]}
		for range 1 + r.IntN(3) {
			parts = append([]string{label()}, parts...)
		}
		return strings.Join(parts, ".")
	}
	exactRef := map[string]bool{}
	suffixRef := map[string]bool{}
	var eb ExactBuilder
	var sb SuffixBuilder
	for range 20000 {
		n := name()
		exactRef[n] = true
		eb.Add(n, 1)
		m := name()
		suffixRef[m] = true
		sb.Add(m, 1)
	}
	e, s := eb.Build(), sb.Build()
	if e.Len() != len(exactRef) || s.Len() != len(suffixRef) {
		t.Fatalf("Len mismatch: exact %d/%d suffix %d/%d", e.Len(), len(exactRef), s.Len(), len(suffixRef))
	}
	for range 200000 {
		q := name()
		if r.IntN(4) == 0 {
			q = label() + "." + q
		}
		if _, ok := e.Match(q); ok != exactRef[q] {
			t.Fatalf("Exact.Match(%q) = %v, want %v", q, ok, exactRef[q])
		}
		want := ""
		labels := strings.Split(q, ".")
		for k := 1; k <= len(labels); k++ { // broadest first: shortest suffix
			if c := strings.Join(labels[len(labels)-k:], "."); suffixRef[c] {
				want = c
				break
			}
		}
		base, _, ok := s.Match(q)
		if ok != (want != "") || base != want {
			t.Fatalf("Suffix.Match(%q) = (%q, %v), want %q", q, base, ok, want)
		}
	}
}

func TestLookupsDoNotAllocate(t *testing.T) {
	var eb ExactBuilder
	var sb SuffixBuilder
	var ib IPSetBuilder
	for i := range 1000 {
		eb.Add(fmt.Sprintf("host%d.example.com", i), 1)
		sb.Add(fmt.Sprintf("zone%d.example.net", i), 1)
	}
	ib.Add(netip.MustParseAddr("192.0.2.1"), 1)
	e, s, ips := eb.Build(), sb.Build(), ib.Build()
	var rb RegexBuilder
	if err := rb.Add(`^ads?[0-9]+\.`, 1); err != nil {
		t.Fatal(err)
	}
	re := rb.Build()
	addr := netip.MustParseAddr("192.0.2.1")
	names := []string{"host5.example.com", "nothere.example.com", "a.b.zone7.example.net", "x.y.z.example.net", "ad12.example"}
	allocs := testing.AllocsPerRun(1000, func() {
		for _, n := range names {
			e.Match(n)
			s.Match(n)
			re.Match(n)
		}
		ips.Match(addr)
	})
	if allocs != 0 {
		t.Fatalf("lookups allocated %.1f times per run, want 0", allocs)
	}
}

func TestFilterFalsePositiveRate(t *testing.T) {
	const n = 100000
	f := newFilter(n)
	for i := range n {
		f.add(Hash(fmt.Sprintf("in-%d.example", i)))
	}
	for i := range n {
		if !f.mayContain(Hash(fmt.Sprintf("in-%d.example", i))) {
			t.Fatalf("false negative for key %d", i)
		}
	}
	fp := 0
	const probes = 200000
	for i := range probes {
		if f.mayContain(Hash(fmt.Sprintf("out-%d.example", i))) {
			fp++
		}
	}
	rate := float64(fp) / probes
	t.Logf("false-positive rate %.3f%% at %d bits/key, %d probes", 100*rate, filterBitsPerKey, filterProbes)
	if rate > 0.02 {
		t.Fatalf("false-positive rate %.3f, want <= 0.02", rate)
	}
	var empty filter
	if empty.mayContain(1) {
		t.Fatal("empty filter reported a member")
	}
}

func TestRegex(t *testing.T) {
	b := RegexBuilder{HardCap: 3}
	if err := b.Add(`^ad[sx]?[0-9]*\.`, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(`^ad[sx]?[0-9]*\.`, 1); err != nil {
		t.Fatalf("duplicate Add returned %v", err)
	}
	if err := b.Add(`TRACK(er|ing)`, 2); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(`(`, 1); err == nil {
		t.Fatal("invalid regex accepted")
	}
	if err := b.Add(`^banner`, 3); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(`^overflow`, 3); !errors.Is(err, ErrRegexCap) {
		t.Fatalf("Add beyond cap = %v, want ErrRegexCap", err)
	}
	if b.Dropped() != 1 || b.Len() != 3 {
		t.Fatalf("Dropped() = %d, Len() = %d, want 1, 3", b.Dropped(), b.Len())
	}
	r := b.Build()
	tests := []struct {
		name, src string
		tag       uint8
		ok        bool
	}{
		{"ads.example.com", `^ad[sx]?[0-9]*\.`, 1, true},
		{"ad12.example.com", `^ad[sx]?[0-9]*\.`, 1, true},
		{"bad.example.com", "", 0, false},
		{"mytracker.example", `TRACK(er|ing)`, 2, true}, // compiled case-insensitively
		{"banner.example", `^banner`, 3, true},
		{"overflow.example", "", 0, false},
	}
	for _, tc := range tests {
		src, tag, ok := r.Match(tc.name)
		if ok != tc.ok || src != tc.src || tag != tc.tag {
			t.Errorf("Match(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.name, src, tag, ok, tc.src, tc.tag, tc.ok)
		}
	}
	if r.Bytes() <= 0 {
		t.Error("Bytes() <= 0")
	}
}

func TestRequiredLiteral(t *testing.T) {
	tests := []struct{ expr, want string }{
		{`^ads\.example\.com$`, "ads.example.com"},
		{`^ad[sx]?[0-9]*\.`, "ad"},
		{`(\.|^)doubleclick\.net$`, "doubleclick.net"},
		{`tracker|ads`, ""},
		{`(?i)TRACK(er|ing)`, "track"},
		{`(foo)+bar`, "foo"},
		{`x{2,}yz`, "yz"},
		{`(abc)?def`, "def"},
		{`a*`, ""},
		{`(`, ""},
	}
	for _, tc := range tests {
		if got := RequiredLiteral(tc.expr); got != tc.want {
			t.Errorf("RequiredLiteral(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

func TestIPSet(t *testing.T) {
	var b IPSetBuilder
	b.Add(netip.MustParseAddr("192.0.2.1"), 1)
	b.Add(netip.MustParseAddr("2001:db8::1"), 2)
	b.Add(netip.MustParseAddr("::ffff:198.51.100.7"), 3)
	b.Add(netip.MustParseAddr("192.0.2.1"), 9)
	if b.Add(netip.Addr{}, 1) {
		t.Fatal("zero Addr accepted")
	}
	s := b.Build()
	if s.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", s.Len())
	}
	tests := []struct {
		addr string
		tag  uint8
		ok   bool
	}{
		{"192.0.2.1", 1, true},
		{"::ffff:192.0.2.1", 1, true},
		{"2001:db8::1", 2, true},
		{"fe80::1%eth0", 0, false},
		{"198.51.100.7", 3, true},
		{"192.0.2.2", 0, false},
	}
	for _, tc := range tests {
		tag, ok := s.Match(netip.MustParseAddr(tc.addr))
		if ok != tc.ok || tag != tc.tag {
			t.Errorf("Match(%s) = (%d, %v), want (%d, %v)", tc.addr, tag, ok, tc.tag, tc.ok)
		}
	}
	if _, ok := s.Match(netip.Addr{}); ok {
		t.Error("zero Addr matched")
	}
}
