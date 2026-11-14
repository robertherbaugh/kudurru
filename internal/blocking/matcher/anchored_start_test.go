package matcher

import (
	"fmt"
	"strings"
	"testing"
)

func TestAutomatonAnchoredStartsOnlyAtInitialBoundary(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			expressions := []string{`^a{3}b`, `\Aa{4}c`}
			if mixed {
				expressions = append(expressions, `(?m)^z`, `a+`)
			}
			a, _ := oracleMachine(t, expressions, 0)
			if a.beginTextOnly == mixed {
				t.Fatal("fixture lost its original public start proof")
			}
			a.step(-1, 'a')
			for _, pc := range a.starts {
				if a.seen[pc] != a.epoch {
					t.Fatal("initial boundary did not visit every original start")
				}
			}
			a.step('a', 'a')
			for _, pc := range a.starts {
				if (a.seen[pc] == a.epoch) != mixed {
					t.Fatal("later boundary did not preserve proven start eligibility")
				}
			}
		})
	}
}

func TestAutomatonAnchoredStartsRetainEveryTagAndFallback(t *testing.T) {
	for _, cache := range []int{0, 512, 65536} {
		t.Run(fmt.Sprint(cache), func(t *testing.T) {
			expressions := []string{`^`, `^$`, `\A\b`, `^a(?:^a)*`, `^(?:a|ab)+$`, `^\x{212A}`, `^\x{FFFD}`}
			for i := range 48 {
				expressions = append(expressions, fmt.Sprintf(`^[a-z]{%d}[0-9]{%d,}\.`, 3+i%20, 1+i/20))
			}
			a, oracles := oracleMachine(t, expressions, cache)
			if !a.beginTextOnly {
				t.Fatal("all original programs must require beginning of text")
			}
			reserved := a.bytes()
			for repeat := 0; repeat < 2; repeat++ {
				for _, name := range []string{"", "a", "aa", "ab", "aba", "a\n", "\na", "K", "\xff", "\xffabc1.", "abc1.", "abc12.example", "prefix.abc1."} {
					if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
						t.Fatalf("original tags changed for %q: got %b want %b", name, got, want)
					}
				}
				for n := 1; n < 30; n++ {
					name := strings.Repeat("a", n) + "123.example"
					if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
						t.Fatalf("fallback tags changed for length %d: got %b want %b", n, got, want)
					}
				}
			}
			if cache == 512 && !a.frozen {
				t.Fatal("tiny cache did not actually saturate")
			}
			if a.bytes() != reserved {
				t.Fatal("start pruning changed original reservation")
			}
			const query = "abcdefghijkl123.example"
			want := oracleMask(oracles, query)
			var got uint64
			if allocs := testing.AllocsPerRun(100, func() { got = a.matchMask(query) }); allocs != 0 || got != want {
				t.Fatal("anchored fallback allocation/result changed", allocs, got, want)
			}
		})
	}
}
