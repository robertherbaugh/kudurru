package matcher

import (
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"testing"
)

func oracleMachine(t testing.TB, expressions []string, cache int) (*automaton, []*regexp.Regexp) {
	t.Helper()
	programs := make([]*syntax.Prog, 0, len(expressions))
	oracles := make([]*regexp.Regexp, 0, len(expressions))
	for _, src := range expressions {
		p, err := compileProgram(src)
		if err != nil {
			t.Fatalf("compile %q: %v", src, err)
		}
		programs = append(programs, p)
		oracles = append(oracles, regexp.MustCompile(src))
	}
	a, err := newAutomaton(programs, cache)
	if err != nil {
		t.Fatal(err)
	}
	return a, oracles
}

func oracleMask(oracles []*regexp.Regexp, name string) uint64 {
	var result uint64
	for i, re := range oracles {
		if re.MatchString(name) {
			result |= uint64(1) << i
		}
	}
	return result
}

func TestAutomatonDifferential(t *testing.T) {
	expressions := []string{"", "a", "^a", "z$", "^$", "a*", "a+?", "(?:a?)*", "(a|ab)+", `\A(?:ab){2,4}\z`, `(?m)^a$`, `\bword\b`, `\B_\B`, `\w+\W\d`, `[^a-z]+`, `(?s)a.*z`, `(?i)k`, `(?i)s`, `\p{Greek}+`, `\P{Latin}+`, "�", "(?i:Ab)(?-i:C)", ".", "(?s:.)", "x|z", "z", "x", "[[:alpha:]]+", "(?:^|a)b?", `a{0,3}$`, `(?m)\A\b`}
	names := []string{"", "a", "aaaa", "ab", "abab", "ababab", "word", " word_", "word\n", "\na\n", "xaz", "a\nz", "zx", "AbC", "abC", "abc", "K", "ſ", "Σσς", "é", "\xffa", "a\xc0\x80", "\x00", "_a_", "9._z", strings.Repeat("ab", 300)}
	rng := rand.New(rand.NewPCG(17, 93))
	alphabet := []byte("abxyz_ .\nABC09")
	for range 500 {
		name := make([]byte, rng.IntN(70))
		for i := range name {
			name[i] = alphabet[rng.IntN(len(alphabet))]
		}
		names = append(names, string(name))
	}
	for _, cache := range []int{0, 512, 4096, 65536} {
		t.Run(fmt.Sprint(cache), func(t *testing.T) {
			a, oracles := oracleMachine(t, expressions, cache)
			for range 2 {
				for _, name := range names {
					if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
						t.Fatalf("%q got %032b want %032b", name, got, want)
					}
				}
			}
		})
	}
}

func TestAutomatonOrderAndSaturation(t *testing.T) {
	a, _ := oracleMachine(t, []string{"z", "a", "a|z", "^a.*z$"}, 65536)
	mask := a.matchMask("az")
	if mask != 15 || bits.TrailingZeros64(mask) != 0 {
		t.Fatalf("insertion-order result %b", mask)
	}
	builds := a.builds
	for range 10 {
		if a.matchMask("az") != mask {
			t.Fatal("warm result changed")
		}
	}
	if builds == 0 || a.builds != builds {
		t.Fatalf("no lazy reuse: %d -> %d", builds, a.builds)
	}
	b, oracles := oracleMachine(t, []string{`(a|b)*a(a|b){9}$`, `a$`}, 512)
	for i := range 2048 {
		name := fmt.Sprintf("%011b", i)
		name = strings.NewReplacer("0", "a", "1", "b").Replace(name)
		if got, want := b.matchMask(name), oracleMask(oracles, name); got != want {
			t.Fatalf("saturated %q %b != %b", name, got, want)
		}
	}
	if !b.frozen {
		t.Fatal("fixture did not saturate")
	}
	builds = b.builds
	for range 5 {
		b.matchMask("abbbbababab")
	}
	if b.builds != builds {
		t.Fatal("admission continued after freeze")
	}
}

func TestAutomatonTransitionTableSaturation(t *testing.T) {
	a, oracles := oracleMachine(t, []string{asciiDistinctSaturationExpression()}, 512)
	proof := asciiProofWork{}
	if !a.refineASCIIClasses(&proof) || asciiClassCount(a) != 128 {
		t.Fatal("saturation fixture did not prove all 128 distinct ASCII classes")
	}
	for i := range 128 {
		name := string(rune(i))
		if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
			t.Fatalf("table saturation %q %b != %b", name, got, want)
		}
	}
	if !a.frozen {
		t.Fatal("bounded transition admission did not freeze")
	}
	if a.stateCount == len(a.states) || a.pcCount == len(a.pcs) {
		t.Fatal("fixture exhausted state/PC capacity instead of transition probes")
	}
	occupied := 0
	for _, transition := range a.transitions {
		if transition.from != 0 {
			occupied++
		}
	}
	if occupied != len(a.transitions) {
		t.Fatal("small transition table did not reach its bounded probe refusal", occupied)
	}
	builds := a.builds
	for _, name := range []string{"", "a", "\xff", "\n", "~\x0000", "~\x7f7f"} {
		if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
			t.Fatal("frozen table changed language")
		}
	}
	if a.builds != builds {
		t.Fatal("frozen transition table admitted more entries")
	}
}

func TestAutomatonConcurrentAndAllocation(t *testing.T) {
	for _, cache := range []int{0, 512, 65536} {
		a, oracles := oracleMachine(t, []string{`(?i)^a.*z$`, `\bword\b`, "a?", `(?:a|b){3,9}$`}, cache)
		name := "abbbaz"
		want := oracleMask(oracles, name)
		// AllocsPerRun warms its callback once; reset admission inside the
		// callback to measure genuinely cold execution on every iteration.
		if cache != 0 {
			if n := testing.AllocsPerRun(20, func() {
				clear(a.transitions)
				clear(a.stateSlots)
				a.stateCount = 1
				a.pcCount = 0
				a.frozen = false
				if a.matchMask(name) != want {
					panic("cold result")
				}
			}); n != 0 {
				t.Fatalf("cold cache %d: %g allocations", cache, n)
			}
		}
		if n := testing.AllocsPerRun(100, func() {
			if a.matchMask(name) != want {
				panic("result")
			}
		}); n != 0 {
			t.Fatalf("cache %d: %g allocations", cache, n)
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for range 100 {
					if a.bytes() < 1 {
						t.Error("missing immutable reservation")
					}
					for _, input := range []string{"", name, "word", "abab", "\xff", "Σσ"} {
						if got, want := a.matchMask(input), oracleMask(oracles, input); got != want {
							t.Errorf("%q %b != %b", input, got, want)
						}
					}
				}
			})
		}
		wg.Wait()
	}
}

func TestAutomatonProgramLimits(t *testing.T) {
	for _, src := range []string{strings.Repeat("a", expressionSourceLimit+1), strings.Repeat(`(?:a{1000})`, 40)} {
		if _, err := compileProgram(src); !errors.Is(err, ErrProgramBudget) {
			t.Fatalf("resource error = %v", err)
		}
	}
	if parseInputFits(strings.Repeat(`\pL`, propertyEscapeLimit+1)) {
		t.Fatal("unbounded pre-parser property expansion")
	}
	if !parseInputFits(strings.Repeat(`\\pL`, propertyEscapeLimit+1)) {
		t.Fatal("escaped backslash counted as property")
	}
	if _, err := compileProgram("["); err == nil || errors.Is(err, ErrProgramBudget) {
		t.Fatalf("syntax error = %v", err)
	}
	if boundedAdd(programInstLimit+100, 1, programInstLimit) != programInstLimit+1 {
		t.Fatal("saturating overflow")
	}
}

func FuzzAutomatonRegexp(f *testing.F) {
	for _, seed := range [][2]string{{"a?", ""}, {`(?i)k`, "K"}, {`(?m)^a$`, "\na\n"}, {`\b\w+\b`, "x.y"}, {"a|z", "az"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, src, name string) {
		if len(src) > 256 || len(name) > 512 {
			t.Skip()
		}
		oracle, err := regexp.Compile(src)
		if err != nil {
			t.Skip()
		}
		p, err := compileProgram(src)
		if errors.Is(err, ErrProgramBudget) {
			t.Skip()
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, cache := range []int{0, 512, 4096} {
			a, err := newAutomaton([]*syntax.Prog{p}, cache)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if got := a.matchMask(name) != 0; got != oracle.MatchString(name) {
					t.Fatalf("%q on %q = %v", src, name, got)
				}
			}
		}
	})
}
