package matcher

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func regexSparseSources() []string {
	return []string{`^aaaa\.hit$`, `^[0-9]+$`, `^bbbb\.hit$`, `^[a-z]+$`, `^.{3}$`, `^cccc$`}
}

func TestRegexSparseSharedBatchRetainsOriginalOrder(t *testing.T) {
	for _, name := range []string{"aaaa.hit", "bbbb.hit", "123", "abc", "cccc", "---", "no-match!"} {
		t.Run(name, func(t *testing.T) {
			r, oracles := buildRegexDispatchOracle(t, regexSparseSources())
			if r.unindexedBatches != 1 || !slices.Equal(r.batchIDs[0], []int32{1, 3, 4}) || len(r.always) != 3 {
				t.Fatal("fixture did not retain the interleaved original unindexed IDs")
			}
			programBytes, bytes := r.ProgramBytes(), r.Bytes()
			src, tag, ok := r.Match(name)
			var want regexDispatchHit
			wantOK := false
			for _, oracle := range oracles {
				if oracle.re.MatchString(name) {
					want, wantOK = oracle.hit, true
					break
				}
			}
			if ok != wantOK || ok && (regexDispatchHit{src, tag}) != want {
				t.Fatal("shared batch changed earliest original source/tag", src, tag, ok, want)
			}
			if (r.batches[0].epoch != 0) != (name != "aaaa.hit") {
				t.Fatal("shared execution did not wait for its original position")
			}
			for _, id := range r.always {
				if r.entries[id].re.machine.epoch != 0 || r.entries[id].re.machine.builds != 0 {
					t.Fatal("fully admitted shared member also executed individually", id)
				}
			}
			if r.ProgramBytes() != programBytes || r.Bytes() != bytes {
				t.Fatal("query changed retained reservation")
			}
			assertRegexDispatchOracle(t, r, oracles, name)
		})
	}
}

func TestRegexSparsePartialAdmissionKeepsIndividuals(t *testing.T) {
	sources := regexSparseSources()
	sources[1] = `^x[a-z]+$`
	r, oracles := buildRegexDispatchOracle(t, sources)
	if r.unindexedBatches != 1 || r.entries[1].lit != "x" {
		t.Fatal("fixture did not retain a real excluded short literal")
	}
	if src, tag, ok := r.Match("abc"); !ok || (regexDispatchHit{src, tag}) != oracles[3].hit {
		t.Fatal("partial literal admission lost earliest eligible rule", src, tag, ok)
	}
	if r.batches[0].epoch != 0 || r.batches[0].builds != 0 || r.entries[1].re.machine.epoch != 0 || r.entries[3].re.machine.epoch == 0 {
		t.Fatal("partially admitted group ran or skipped its original individual")
	}
	assertRegexDispatchOracle(t, r, oracles, "abc")
}

func TestRegexSparseBatchCacheFallbackAndUnsafeOracle(t *testing.T) {
	for _, capacity := range []int{0, 512, 64 << 10} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			sources := regexSparseSources()
			// Distinct empty-literal expressions keep the sparse route while
			// exercising original-ID mask bits beyond the first three members.
			for i := 4; i < 10; i++ {
				sources = append(sources, fmt.Sprintf(`^[a-z.]{%d}$`, i))
			}
			sources = append(sources, `(?:)`)
			r, oracles := buildRegexDispatchOracle(t, sources)
			setRegexIndexedLaneCaches(t, r, regexIndexedLaneCacheMode{bytes: capacity})
			if len(r.always) != 10 || r.unindexedBatches != 1 {
				t.Fatal("fixture escaped the sparse unindexed batch")
			}
			for repeat := 0; repeat < 2; repeat++ {
				for _, name := range []string{"", "abc", "aaaa.hit", "bbbb.hit", "123", "cccc", "ABC", "K", "\xffabc", "no-match!", strings.Repeat("a", 20)} {
					assertRegexDispatchOracle(t, r, oracles, name)
				}
				for n := 1; n < 16; n++ {
					assertRegexDispatchOracle(t, r, oracles, strings.Repeat("a", n))
				}
			}
			if capacity == 512 && !r.batches[0].frozen {
				t.Fatal("tiny enabled cache did not reach actual saturation")
			}
		})
	}
}

func TestRegexSparseBatchZeroQueryAllocations(t *testing.T) {
	for _, capacity := range []int{0, 512, 64 << 10} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			r, oracles := buildRegexDispatchOracle(t, regexSparseSources())
			setRegexIndexedLaneCaches(t, r, regexIndexedLaneCacheMode{bytes: capacity})
			for _, name := range []string{"aaaa.hit", "bbbb.hit", "abc", "123", "no-match!"} {
				assertRegexDispatchOracle(t, r, oracles, name)
				wantSource, wantTag, wantOK := r.Match(name)
				var src string
				var tag uint8
				var ok bool
				if n := testing.AllocsPerRun(100, func() { src, tag, ok = r.Match(name) }); n != 0 {
					t.Fatal("shared sparse lookup allocated", name, n)
				}
				if src != wantSource || tag != wantTag || ok != wantOK {
					t.Fatal("allocation fixture changed original result")
				}
			}
		})
	}
}
