package matcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func BenchmarkLazyDFA(b *testing.B) {
	for _, count := range []int{50, 100, 500, 1000} {
		for _, mode := range []string{"warm", "saturated", "cold"} {
			b.Run(fmt.Sprintf("unindexed-%d/%s", count, mode), func(b *testing.B) {
				builder := RegexBuilder{Budget: NewProgramBudget(DefaultProgramBytes)}
				for i := range count {
					if err := builder.Add(fmt.Sprintf(`^(?:a|b){%d}$`, i%30+1)+strings.Repeat("()", i/30), 1); err != nil {
						b.Fatal(err)
					}
				}
				r, err := builder.BuildChecked()
				if err != nil {
					b.Fatal(err)
				}
				name := "abbaabbaabbaabbaabbaabba.no-match"
				r.Match(name)
				if mode == "saturated" {
					for _, a := range r.batches {
						a.frozen = true
						clear(a.transitions) // require exact missing-transition fallback
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if mode == "cold" {
						for _, a := range r.batches {
							clear(a.transitions)
							clear(a.stateSlots)
							a.stateCount = 1
							a.pcCount = 0
							a.frozen = false
						}
					}
					r.Match(name)
				}
				b.StopTimer()
				b.ReportMetric(float64(builder.Budget.Used()), "reserved-B")
			})
		}
	}
}

// This independent stdlib reference is the former unindexed dispatch model,
// using expressions with no required literal at all (not even a cheap absent
// one-byte Contains prefilter). It is not a refreshed global baseline.
func BenchmarkUnindexedRegexpReference(b *testing.B) {
	for _, count := range []int{50, 100, 500, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var expressions []*regexp.Regexp
			for i := range count {
				expressions = append(expressions, regexp.MustCompile(fmt.Sprintf(`^(?:a|b){%d}$`, i%30+1)+strings.Repeat("()", i/30)))
			}
			name := "abbaabbaabbaabbaabbaabba.no-match"
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				for _, expression := range expressions {
					if expression.MatchString(name) {
						b.Fatal("unexpected reference hit")
					}
				}
			}
		})
	}
}

func BenchmarkLazyDFAParallel(b *testing.B) {
	for _, saturated := range []bool{false, true} {
		b.Run(fmt.Sprintf("saturated=%v", saturated), func(b *testing.B) {
			var builder RegexBuilder
			for i := range 1000 {
				if err := builder.Add(fmt.Sprintf(`^(?:a|b){%d}$`, i%30+1)+strings.Repeat("()", i/30), 1); err != nil {
					b.Fatal(err)
				}
			}
			r := builder.Build()
			name := "abbaabbaabbaabbaabbaabba.no-match"
			r.Match(name)
			if saturated {
				for _, a := range r.batches {
					a.frozen = true
					clear(a.transitions)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					r.Match(name)
				}
			})
		})
	}
}

// A new test process isolates parser/compile peak evidence from earlier tests.
// Measurements are observations for the pinned Go version, not a replacement
// for admission limits or a claim that RSS equals the retained reservation.
func TestExpressionCompilePeak(t *testing.T) {
	fixture := os.Getenv("KUDURRU_REGEX_PEAK_FIXTURE")
	if fixture == "" {
		for _, name := range []string{"literal64k", "unicode-classes", "unicode-boundary", "repeat-expansion", "repeat-refused", "repeat-accepted", "modifier2501", "modifier10000-budget128"} {
			t.Run(name, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestExpressionCompilePeak$", "-test.v") //nolint:gosec // own test binary and fixed arguments
				cmd.Env = append(os.Environ(), "KUDURRU_REGEX_PEAK_FIXTURE="+name)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("isolated peak fixture: %v", err)
				}
				t.Log(strings.TrimSpace(string(out)))
			})
		}
		return
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	budget := NewProgramBudget(DefaultProgramBytes)
	source := ""
	wantRefusal := true
	switch fixture {
	case "literal64k":
		source = strings.Repeat("a", 64<<10)
	case "unicode-classes":
		source = strings.Repeat(`\p{L}`, 1000)
	case "unicode-boundary":
		source = strings.Repeat(`\pL`, propertyEscapeLimit)
	case "repeat-expansion":
		source = strings.Repeat(`a{1000}`, 32)
	case "repeat-refused":
		source = strings.Repeat(`a{1000}`, 10)
	case "repeat-accepted":
		source = strings.Repeat(`a{1000}`, 6)
		wantRefusal = false
	case "modifier2501", "modifier10000-budget128":
		count := 2501
		if fixture == "modifier10000-budget128" {
			count = 10000
			// Measure the explicitly proposed list/candidate ceiling without
			// silently changing the current production default.
			budget = NewProgramBudget(128 << 20)
		}
		wantRefusal = false
		var patterns []*Pattern
		for i := range count {
			p, err := CompilePatternWithBudget("||host"+strconv.Itoa(i)+".example^", budget)
			if err != nil {
				t.Fatal(err)
			}
			patterns = append(patterns, p)
		}
		p, err := NewPatternSet(patterns, budget)
		if err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(p)
	default:
		t.Fatal("unknown peak fixture")
	}
	var compileErr error
	if source != "" {
		_, compileErr = compileExpression(source, budget)
	}
	if wantRefusal {
		if !errors.Is(compileErr, ErrProgramBudget) {
			t.Fatalf("fixture=%s requires typed resource refusal, got %v", fixture, compileErr)
		}
	} else if compileErr != nil {
		t.Fatalf("fixture=%s requires accepted compilation: %v", fixture, compileErr)
	}
	runtime.ReadMemStats(&after)
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	peak := "unavailable"
	for line := range strings.SplitSeq(string(status), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			peak = strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	t.Logf("fixture=%s source=%d budget=%d reserved=%d alloc_delta=%d heap_inuse=%d peak_rss=%s elapsed=%s resource_refusal=%v", fixture, len(source), budget.limit, budget.Used(), after.TotalAlloc-before.TotalAlloc, after.HeapInuse, peak, time.Since(start), compileErr != nil)
}
