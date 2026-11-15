package matcher

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestExpressionDiagnosticAdmissionAndCancellation(t *testing.T) {
	e, err := CompileRegex("a.*z")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if matched, err := e.MatchStringContext(ctx, "abz"); matched || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became a match/miss: %v %v", matched, err)
	}
	e.machine.mu.Lock()
	matched, err := e.MatchStringContext(t.Context(), "abz")
	e.machine.mu.Unlock()
	if matched || !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("busy admission became a language result: %v %v", matched, err)
	}
	if matched, err := e.MatchStringContext(t.Context(), "abz"); !matched || err != nil {
		t.Fatal("refused diagnostics changed exact executor", err)
	}
}

func TestProductionExpressionParityAndShards(t *testing.T) {
	var builder RegexBuilder
	var sources []string
	for i := range 150 {
		src := fmt.Sprintf(`a(?:b|c){%d}z`, i%5)
		// Distinct empty captures preserve language but force distinct IDs.
		src += strings.Repeat("()", i)
		sources = append(sources, src)
		if err := builder.Add(src, uint8(i%255)); err != nil {
			t.Fatal(err)
		}
	}
	re := builder.Build()
	for _, name := range []string{"", "az", "abz", "acbcz", "abbbbz", "zabbbzaz", "\xffaz", "Kaz"} {
		var want, got []string
		for _, src := range sources {
			if regexp.MustCompile(src).MatchString(name) {
				want = append(want, src)
			}
		}
		re.Visit(name, func(src string, _ uint8) bool { got = append(got, src); re.Match(name); return true })
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("all IDs/order %q: %d != %d", name, len(got), len(want))
		}
		first, _, ok := re.Match(name)
		if ok != (len(want) != 0) || ok && first != want[0] {
			t.Fatal("first ID differs from all-match oracle")
		}
	}
	for _, src := range []string{`ABC`, `\P{Latin}`, `(?i)k`, `(?i)s`, `(?i:ab)(?-i:C)`, `\bword\b`, `K`, `foo(?i:K)`, `(?i)\x{017f}`, `foo(?i:\x{212a})`} {
		e, err := CompileRegex(src)
		if err != nil {
			t.Fatal(err)
		}
		effective := src
		if hasUpperASCII(src) {
			effective = "(?i)" + effective
		}
		oracle := regexp.MustCompile(effective)
		var b RegexBuilder
		if err := b.Add(src, 7); err != nil {
			t.Fatal(err)
		}
		r := b.Build()
		for _, name := range []string{"abc", "ABC", "aB", "abC", "abcc", "K", "k", "ſ", "s", "Σ", "fooK", "fook", " word ", "\xff"} {
			want := oracle.MatchString(name)
			_, _, got := r.Match(name)
			if got != want || e.MatchString(name) != want {
				t.Fatalf("%q %q parity", src, name)
			}
		}
	}
}

func TestPatternSetDenseContextIndependentLanguage(t *testing.T) {
	var patterns []*Pattern
	for i := range 130 {
		p, err := CompilePattern(fmt.Sprintf(`/a{0,%d}z/`, i+1))
		if err != nil {
			t.Fatal(err)
		}
		patterns = append(patterns, p)
	}
	set, err := NewPatternSet(patterns, NewProgramBudget(DefaultProgramBytes))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "az", "zz", strings.Repeat("a", 129) + "z", "\xffaz"} {
		last := -1
		count := 0
		set.VisitMatches(name, func(id int) {
			if id <= last || !patterns[id].Match(name) {
				t.Fatal("incorrect batched language ID")
			}
			last = id
			count++
		})
		want := 0
		for _, p := range patterns {
			if p.Match(name) {
				want++
			}
		}
		if count != want {
			t.Fatalf("%q hits %d want %d", name, count, want)
		}
	}
}
