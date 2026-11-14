package matcher

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Reset inside a cold timed iteration, rather than letting the benchmark
// harness's warmup silently turn construction into a warm-cache measurement.
func resetBenchmarkBatches(batches []*automaton) {
	for _, a := range batches {
		clear(a.transitions)
		clear(a.stateSlots)
		a.stateCount = 1
		a.pcCount = 0
		a.builds = 0
		a.frozen = false
	}
}

func saturateBenchmarkBatches(batches []*automaton) {
	for _, a := range batches {
		a.frozen = true
		clear(a.transitions) // measure exact fallback on every missing transition
	}
}

// Every expression has the same required literal. The ten character classes
// encode a distinct language without introducing a discriminating literal;
// the early/late cases therefore really exercise dense candidate dispatch.
func denseBenchmarkExpression(id int) (source, suffix string) {
	var expression, name strings.Builder
	expression.WriteString(`^shared[.]example`)
	for bit := 9; bit >= 0; bit-- {
		if id&(1<<bit) == 0 {
			expression.WriteString(`[ax]`)
			name.WriteByte('a')
		} else {
			expression.WriteString(`[bx]`)
			name.WriteByte('b')
		}
	}
	expression.WriteByte('$')
	return expression.String(), name.String()
}

func BenchmarkLazyDFADense(b *testing.B) {
	for _, count := range []int{50, 100, 500, 1000} {
		for _, shape := range []string{"miss", "early-hit", "late-hit"} {
			for _, mode := range []string{"warm", "cold", "saturated"} {
				b.Run(fmt.Sprintf("rules-%d/%s/%s", count, shape, mode), func(b *testing.B) {
					builder := RegexBuilder{Budget: NewProgramBudget(DefaultProgramBytes)}
					name, want := "shared.examplecccccccccc", ""
					for id := range count {
						source, suffix := denseBenchmarkExpression(id)
						if RequiredLiteral(source) != "shared.example" {
							b.Fatal("fixture acquired a discriminating shortcut")
						}
						if err := builder.Add(source, 1); err != nil {
							b.Fatal(err)
						}
						if shape == "early-hit" && id == 0 || shape == "late-hit" && id == count-1 {
							name, want = "shared.example"+suffix, source
						}
					}
					r, err := builder.BuildChecked()
					if err != nil {
						b.Fatal(err)
					}
					check := func() {
						got, _, matched := r.Match(name)
						if got != want || matched != (want != "") {
							b.Fatal("dense workload returned the wrong original rule")
						}
					}
					check()
					if mode == "saturated" {
						saturateBenchmarkBatches(r.batches)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if mode == "cold" {
							resetBenchmarkBatches(r.batches)
						}
						r.Match(name)
					}
					b.StopTimer()
					check()
					b.ReportMetric(float64(builder.Budget.Used()), "reserved-B")
				})
			}
		}
	}
}

// Inputs intentionally exceed a DNS wire name. The matcher library retains
// its exact arbitrary-input language and linear fallback-work contract.
func BenchmarkLazyDFALongInput(b *testing.B) {
	for _, length := range []int{256, 4096, 16384} {
		for _, mode := range []string{"warm", "cold", "saturated"} {
			b.Run(fmt.Sprintf("bytes-%d/%s", length, mode), func(b *testing.B) {
				builder := RegexBuilder{Budget: NewProgramBudget(DefaultProgramBytes)}
				for id := range 32 {
					source := fmt.Sprintf(`^(?:a|b)*a(?:a|b){%d}$`, id%12+1) + strings.Repeat("()", id/12)
					if err := builder.Add(source, 1); err != nil {
						b.Fatal(err)
					}
				}
				r, err := builder.BuildChecked()
				if err != nil {
					b.Fatal(err)
				}
				name := strings.Repeat("a", length-1) + "x"
				if _, _, matched := r.Match(name); matched {
					b.Fatal("unexpected long-input hit")
				}
				if mode == "saturated" {
					saturateBenchmarkBatches(r.batches)
				}
				b.SetBytes(int64(len(name)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if mode == "cold" {
						resetBenchmarkBatches(r.batches)
					}
					r.Match(name)
				}
				b.StopTimer()
				b.ReportMetric(float64(builder.Budget.Used()), "reserved-B")
			})
		}
	}
}

func BenchmarkLazyDFAStateGrowth(b *testing.B) {
	for _, cache := range []int{512, 4096, 65536} {
		for _, mode := range []string{"cold-to-saturation", "saturated"} {
			b.Run(fmt.Sprintf("cache-%d/%s", cache, mode), func(b *testing.B) {
				a, oracles := oracleMachine(b, []string{`(a|b)*a(a|b){9}$`, `a$`}, cache)
				var names []string
				var wants []uint64
				for id := range 2048 {
					name := fmt.Sprintf("%011b", id)
					name = strings.NewReplacer("0", "a", "1", "b").Replace(name)
					names = append(names, name)
					wants = append(wants, oracleMask(oracles, name))
				}
				batches := []*automaton{a}
				if mode == "saturated" {
					for id, name := range names {
						if a.matchMask(name) != wants[id] {
							b.Fatal("state-growth warmup disagrees with independent oracle")
						}
					}
					if !a.frozen {
						b.Fatal("pathological workload did not exhaust admission")
					}
				}
				var admitted uint64
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if mode == "cold-to-saturation" {
						resetBenchmarkBatches(batches)
					}
					before := a.builds
					for id, name := range names {
						if a.matchMask(name) != wants[id] {
							b.Fatal("state-growth result disagrees with independent oracle")
						}
					}
					admitted += a.builds - before
					if !a.frozen {
						b.Fatal("expected bounded admission freeze")
					}
				}
				b.StopTimer()
				if mode == "saturated" && admitted != 0 {
					b.Fatal("admission continued after freeze")
				}
				b.ReportMetric(float64(admitted)/float64(b.N), "transitions/op")
				b.ReportMetric(float64(a.bytes()), "reserved-B")
				b.ReportMetric(float64(len(names)), "names/op")
			})
		}
	}
}

// Keep the encoded identity within the ten-class workload's language space.
func TestDenseBenchmarkLanguageIdentity(t *testing.T) {
	for _, id := range []int{0, 1, 49, 99, 499, 999} {
		source, suffix := denseBenchmarkExpression(id)
		a, oracle := oracleMachine(t, []string{source}, 512)
		for _, name := range []string{"shared.example" + suffix, "shared.examplecccccccccc"} {
			if a.matchMask(name) != oracleMask(oracle, name) {
				t.Fatal("dense fixture identity mismatch for " + strconv.Itoa(id))
			}
		}
	}
}
