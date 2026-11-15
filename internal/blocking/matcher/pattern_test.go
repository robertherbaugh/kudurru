package matcher

import (
	"testing"
)

func TestPatternBoundariesAndMidPatternWildcards(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		matches       bool
	}{
		{"||ads*.example^", "ads123.example", true}, {"||ads*.example^", "sub.ads123.example", true},
		{"||ads*.example^", "notads123.example", false}, {"||ads*.example^", "ads123.example.evil", false},
		{"|ads*.example|", "ads1.example", true}, {"|ads*.example|", "sub.ads1.example", false},
		{"|ads", "ads123.example", true}, {"|ads", "sub.ads123.example", false},
		{"ad*banner", "sub.ad-big-banner", true}, {"ad*banner", "adbanner.other", true},
		{"*.*.example", "two.one.example", true}, {"*.*.example", "one.example", false},
		{"||*.example^", "example", true}, // documented wildcard apex semantics
		{"/^one(foo|bar)?\\.test$/", "one.test", true}, {"/^one(foo|bar)?\\.test$/", "onebar.test", true},
	} {
		p, err := CompilePattern(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Match(tc.name); got != tc.matches {
			t.Errorf("%s on %s=%v", tc.pattern, tc.name, got)
		}
	}
}

func TestPatternIndexNeverTruncatesDenseCandidates(t *testing.T) {
	var patterns []*Pattern
	for i := 0; i < 3000; i++ {
		p, err := CompilePattern("||shared.example^")
		if err != nil {
			t.Fatal(err)
		}
		patterns = append(patterns, p)
	}
	idx := NewPatternIndex(patterns)
	seen := make(map[int]int)
	idx.Visit("shared.shared.example", func(i int) { seen[i]++ })
	if len(seen) != 3000 {
		t.Fatalf("candidate bucket truncated to %d", len(seen))
	}
	for i, count := range seen {
		if count != 1 {
			t.Fatalf("candidate %d visited %d times", i, count)
		}
	}
	if !patterns[2999].Match("shared.shared.example") {
		t.Fatal("last applicable candidate lost")
	}
}

func TestPatternIndexConservativeForOptionalAndAlternativeLiterals(t *testing.T) {
	var patterns []*Pattern
	for _, expression := range []string{"/^(alpha|bravo)\\.test$/", "/^(prefix)?delta$/", "/^.{1,6}$/", "||UPPER*.example^", "||*.test^"} {
		p, err := CompilePattern(expression)
		if err != nil {
			t.Fatal(err)
		}
		patterns = append(patterns, p)
	}
	idx := NewPatternIndex(patterns)
	for _, name := range []string{"alpha.test", "bravo.test", "delta", "prefixdelta", "abc", "upper1.example", "test", "sub.test"} {
		seen := map[int]bool{}
		idx.Visit(name, func(i int) { seen[i] = true })
		for i, p := range patterns {
			if p.Match(name) && !seen[i] {
				t.Fatalf("shortcut omitted %s pattern %d", name, i)
			}
		}
	}
}
