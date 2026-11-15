package matcher

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
	"unsafe"
)

func TestAutomatonBeginTextProof(t *testing.T) {
	for _, tc := range []struct {
		name        string
		expressions []string
		want        bool
	}{
		{"no-programs", nil, false},
		{"begin-text", []string{`^a`, `\Ab`}, true},
		{"capture-before-anchor", []string{`((^a))`}, true},
		{"begin-line-only", []string{`(?m)^a`}, false},
		{"explicit-text-with-multiline", []string{`(?m)\Aa$`}, true},
		{"mixed-anchors", []string{`^a`, `(?m)^z$`}, false},
		{"unanchored-last", []string{`^a`, `z$`}, false},
		{"unanchored-first", []string{`z$`, `^a`}, false},
		{"optional-anchor", []string{`(?:^a)?b`}, false},
		{"alternative-without-anchor", []string{`(?:^a|b)`}, false},
		{"empty-unanchored", []string{`^a`, ``}, false},
		{"zero-width", []string{`^`, `^$`, `\A\b\B`}, true},
		{"empty-rune-language", []string{`[^\x00-\x{10FFFF}]`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, oracles := oracleMachine(t, tc.expressions, 4096)
			if a.beginTextOnly != tc.want {
				t.Fatalf("required-begin-text proof=%v want%v", a.beginTextOnly, tc.want)
			}
			for _, name := range []string{"", "a", "b", "z", "q\na", "q\nz", "a\n", "\xffz", "Ka"} {
				if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
					t.Fatalf("proof fixture %q tags%b want%b", name, got, want)
				}
				if a.beginTextOnly != tc.want {
					t.Fatal("query changed immutable anchor proof")
				}
			}
		})
	}
	t.Run("public-no-match-sentinel", func(t *testing.T) {
		program, err := syntax.Compile(&syntax.Regexp{Op: syntax.OpNoMatch})
		if err != nil {
			t.Fatal(err)
		}
		if got := program.StartCond(); got != ^syntax.EmptyOp(0) {
			t.Fatalf("public no-match program lacks sentinel: %v", got)
		}
		oracles := []*regexp.Regexp{regexp.MustCompile(`[^\x00-\x{10FFFF}]`)}
		for _, cache := range []int{0, 512, 4096} {
			t.Run(fmt.Sprintf("cache%d", cache), func(t *testing.T) {
				a, err := newAutomaton([]*syntax.Prog{program}, cache)
				if err != nil {
					t.Fatal(err)
				}
				if !a.beginTextOnly {
					t.Fatal("actual no-match sentinel did not permit vacuous proof")
				}
				for range 2 {
					for _, name := range []string{"", "a", "\x00", "\n", "K", "Σσς", "\xff", "a\xc0\x80z"} {
						if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
							t.Fatalf("no-match sentinel %q tags%b want%b", name, got, want)
						}
						if !a.beginTextOnly {
							t.Fatal("query changed immutable no-match proof")
						}
					}
				}
			})
		}
	})
}

// Err remains the real underlying context's result. The optional cancellation
// on a chosen observation makes the final post-termination check deterministic.
type beginTextChecksContext struct {
	context.Context
	cancel          context.CancelFunc
	cancelAt, calls int
}

func (c *beginTextChecksContext) Err() error {
	c.calls++
	if c.cancel != nil && c.calls == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func beginTextPathMachine(t *testing.T, expression, mode string) (*automaton, []*regexp.Regexp) {
	t.Helper()
	expressions := []string{expression}
	cache := 65536
	if mode == "no-cache" {
		cache = 0
	}
	if mode == "saturated" {
		cache = 512
		expressions = append(expressions, asciiDistinctSaturationExpression())
	}
	a, oracles := oracleMachine(t, expressions, cache)
	if !a.beginTextOnly {
		t.Fatal("path fixture has no required-begin-text proof")
	}
	if mode == "warm" || mode == "frozen-cached" {
		a.matchMask("z")
	}
	if mode == "frozen-cached" || mode == "frozen-NFA" {
		a.frozen = true
	}
	if mode == "saturated" {
		for symbol := range 128 {
			a.matchMask(string(rune(symbol)))
		}
		if !a.frozen {
			t.Fatal("actual bounded admission did not saturate")
		}
	}
	return a, oracles
}

func TestAutomatonBeginTextTerminationPaths(t *testing.T) {
	name := "z" + strings.Repeat("a\nK\xff", 1024)
	for _, expression := range []string{`^a`, `^`} {
		for _, mode := range []string{"no-cache", "cold", "warm", "frozen-cached", "frozen-NFA", "saturated"} {
			t.Run(expression+"/"+mode, func(t *testing.T) {
				a, oracles := beginTextPathMachine(t, expression, mode)
				cached := false
				if a.stateCount != 0 {
					transition := a.transition(1, uint16(a.asciiClasses['z']))
					if transition != nil && transition.from != 0 {
						if transition.to == 0 || a.states[transition.to-1].count != 0 {
							t.Fatal("cached fixture lacks an empty successor")
						}
						cached = true
					}
				}
				if (mode == "warm" || mode == "frozen-cached") && !cached {
					t.Fatal("fixture did not exercise cached termination")
				}
				epoch, builds, frozen := a.epoch, a.builds, a.frozen
				ctx := &beginTextChecksContext{Context: t.Context()}
				got, err := a.matchMaskContext(ctx, name)
				if err != nil || got != oracleMask(oracles, name) {
					t.Fatalf("dead-successor tags%b error%v", got, err)
				}
				if ctx.calls != 3 {
					t.Fatalf("executor scanned beyond dead anchored successor: %d context observations", ctx.calls)
				}
				wantSteps := uint32(1)
				if cached {
					wantSteps = 0
				}
				if a.epoch-epoch != wantSteps {
					t.Fatalf("wrong cached/NFA execution: steps%d want%d", a.epoch-epoch, wantSteps)
				}
				if frozen && a.builds != builds {
					t.Fatal("termination restarted frozen admission")
				}
			})
		}
	}
}

func TestAutomatonBeginTextTaggedOracle(t *testing.T) {
	groups := [][]string{
		{`^`, `^$`, `^a`, `^a$`, `^a.*z$`, `\A\bword\b`, `\A\b\B`},
		{`^a*`, `^a*?$`, `\A(?:ab){0,3}\z`, `\A\B`, `\A(?:a|ab)+`},
		{`(?i)\Ak`, `\A\p{Greek}+$`, `\A�`, `\A�$`, `\A[\x00-\x7f]*\z`, `(?m)\Aa$`},
	}
	var allTags []string
	for id := range 64 {
		allTags = append(allTags, "^a"+strings.Repeat("()", id))
	}
	allTags[63] = `^a.*z$`
	groups = append(groups, allTags)
	names := []string{"", "\x00", "\n", "a", "aa", "ab", "ababab", "word", "word!", "a\nz", "abz", "q\na", "K", "k", "Σσς", "\xff", "�", "\xffz", "a\xc0\x80z", "a\x00z", "z" + strings.Repeat("a", 1024), "a" + strings.Repeat("b", 256) + "z", "a" + strings.Repeat("b", 256) + "x"}
	for group, expressions := range groups {
		for _, cache := range []int{0, 512, 4096, 65536} {
			t.Run(fmt.Sprintf("group%d/cache%d", group, cache), func(t *testing.T) {
				a, oracles := oracleMachine(t, expressions, cache)
				if !a.beginTextOnly {
					t.Fatal("tag fixture lacks required-begin-text proof")
				}
				for pass := range 3 {
					if pass == 2 {
						a.frozen = true
					}
					for _, name := range names {
						if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
							t.Fatalf("pass%d %q tagged%b want%b", pass, name, got, want)
						}
					}
				}
			})
		}
	}
}

func TestAutomatonBeginTextMixedAndMultiline(t *testing.T) {
	groups := [][]string{
		{`^a`, `z$`},
		{`z$`, `^a`},
		{`^a`, `(?m)^z$`},
		{`(?m)^a$`, `(?m)^z$`},
		{`^a`, `\bword\b`},
		{`^a`, ``},
		{`(?:^a|z$)`, `^b`},
	}
	for group, expressions := range groups {
		for _, cache := range []int{0, 512, 4096, 65536} {
			t.Run(fmt.Sprintf("group%d/cache%d", group, cache), func(t *testing.T) {
				a, oracles := oracleMachine(t, expressions, cache)
				if a.beginTextOnly {
					t.Fatal("unanchored or multiline-only member gained global proof")
				}
				for pass := range 3 {
					if pass == 2 {
						a.frozen = true
					}
					for _, name := range []string{"", "z", "a", "q\nz", "q\na\nz", "bbz", "b word", "Kz", "\xffz", "a\xc0\x80z", "q" + strings.Repeat("b", 1024) + "z"} {
						if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
							t.Fatalf("pass%d %q mixed tags%b want%b", pass, name, got, want)
						}
					}
				}
			})
		}
	}
}

func TestAutomatonBeginTextCancellation(t *testing.T) {
	for _, expression := range []string{`^a`, `^`} {
		for _, mode := range []string{"no-cache", "cold", "warm", "frozen-cached", "frozen-NFA", "saturated"} {
			t.Run(expression+"/"+mode, func(t *testing.T) {
				a, _ := beginTextPathMachine(t, expression, mode)
				underlying, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx := &beginTextChecksContext{Context: underlying, cancel: cancel, cancelAt: 3}
				got, err := a.matchMaskContext(ctx, "z"+strings.Repeat("a", 4096))
				if got != 0 || !errors.Is(err, context.Canceled) || ctx.calls != 3 {
					t.Fatalf("final cancellation became tags%b error%v checks%d", got, err, ctx.calls)
				}
			})
		}
	}
}

func TestAutomatonBeginTextBudget(t *testing.T) {
	program, err := compileProgram(`^a`)
	if err != nil {
		t.Fatal(err)
	}
	for _, cache := range []int{0, 512, 4096, 65536} {
		full, err := newBudgetedAutomaton([]*syntax.Prog{program}, cache, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(unsafe.Sizeof(*full)) + int64(len(program.Inst))*(int64(unsafe.Sizeof(syntax.Inst{}))+24) + 4
		if cache >= 512 {
			want += int64(cache)
		}
		if full.bytes() != int(want) || !full.beginTextOnly {
			t.Fatal("immutable proof not covered by retained struct reservation", cache, full.bytes(), want)
		}
		root := NewProgramBudget(want)
		budget := root.Child(want)
		a, err := newBudgetedAutomaton([]*syntax.Prog{program}, cache, budget)
		if err != nil || a == nil || !a.beginTextOnly || budget.Used() != want || root.Used() != want {
			t.Fatal("exact retained proof admission failed", cache, err)
		}
		if a.matchMask("a") != 1 || a.matchMask("z-tail") != 0 {
			t.Fatal("byte-limited anchor proof changed language", cache)
		}
		shortRoot := NewProgramBudget(want - 1)
		short := shortRoot.Child(want)
		if got, err := newBudgetedAutomaton([]*syntax.Prog{program}, cache, short); got != nil || !errors.Is(err, ErrProgramBudget) || shortRoot.Used() != 0 || short.Used() != 0 {
			t.Fatal("retained proof shortage was not typed and atomic", cache, err)
		}
	}
}
