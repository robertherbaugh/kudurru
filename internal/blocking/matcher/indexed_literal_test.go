package matcher

import (
	"fmt"
	"strings"
	"testing"
)

func TestRegexIndexedLiteralRefinement(t *testing.T) {
	for _, mode := range regexIndexedLaneCacheModes() {
		t.Run(mode.name, func(t *testing.T) {
			var sources []string
			for i := range 70 {
				sources = append(sources, `[a-z]+z$`+strings.Repeat("()", i))
				sources = append(sources, fmt.Sprintf(`^aaaaab[cd]{%d}$`, i+1))
			}
			sources = append(sources, `(?i)^kilo$`)
			r, oracles := buildRegexDispatchOracle(t, sources)
			setRegexIndexedLaneCaches(t, r, mode)
			programBytes, retainedBytes := r.ProgramBytes(), r.Bytes()
			before := make([]uint32, len(r.batches))
			for i, machine := range r.batches {
				before[i] = machine.epoch
			}
			// An exact selected gram, not merely a bitmap collision, is present.
			// No full indexed required literal is present. Visit must avoid all
			// indexed executors even when the unindexed lane is exhausted.
			if !r.hasIndexedGram("aaaa.test") || r.hasIndexedCandidate("aaaa.test") {
				t.Fatal("fixture did not distinguish gram eligibility from full literal")
			}
			assertRegexDispatchOracle(t, r, oracles, "aaaa.test")
			for i := r.unindexedBatches; i < len(r.batches); i++ {
				if r.batches[i].epoch != before[i] {
					t.Fatal("excluded indexed lane executed", i)
				}
			}
			// A required literal is still only eligibility; final language,
			// ordering, tags and callback stopping belong to the original engine.
			for _, name := range []string{"aaaaab", "aaaaabc", "aaaaabdd", "aaaaabz", "kilo", "KILO", "Kilo", "\xffaaaaabc", "", "safe.test"} {
				assertRegexDispatchOracle(t, r, oracles, name)
			}
			if !r.hasIndexedCandidate("aaaaab") || !r.hasIndexedCandidate("aaaaabc") {
				t.Fatal("present full literal was excluded")
			}
			if r.ProgramBytes() != programBytes || r.Bytes() != retainedBytes {
				t.Fatal("query refinement changed retained accounting")
			}
		})
	}
}
