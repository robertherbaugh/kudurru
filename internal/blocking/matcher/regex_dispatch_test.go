package matcher

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

type regexDispatchHit struct {
	source string
	tag    uint8
}

type regexDispatchOracle struct {
	hit regexDispatchHit
	re  *regexp.Regexp
}

func buildRegexDispatchOracle(t *testing.T, sources []string) (*Regex, []regexDispatchOracle) {
	t.Helper()
	var builder RegexBuilder
	oracles := make([]regexDispatchOracle, 0, len(sources))
	for i, source := range sources {
		tag := uint8(i%251 + 1)
		if err := builder.Add(source, tag); err != nil {
			t.Fatal(err)
		}
		// Independently implement the documented uppercase-source wrapper;
		// oracle matching never calls the tagged engine or its literal proof.
		effective := source
		if strings.ContainsAny(source, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			effective = "(?i)" + source
		}
		oracles = append(oracles, regexDispatchOracle{
			hit: regexDispatchHit{source: source, tag: tag},
			re:  regexp.MustCompile(effective),
		})
	}
	r := builder.Build()
	if r.Len() != len(sources) {
		t.Fatal("fixture lost distinct original rules")
	}
	return r, oracles
}

func assertRegexDispatchOracle(t *testing.T, r *Regex, oracles []regexDispatchOracle, name string) {
	t.Helper()
	var want []regexDispatchHit
	for _, oracle := range oracles {
		if oracle.re.MatchString(name) {
			want = append(want, oracle.hit)
		}
	}
	source, tag, ok := r.Match(name)
	if ok != (len(want) != 0) || ok && (regexDispatchHit{source, tag}) != want[0] {
		t.Fatalf("first original rule for %q = (%q, %d, %v), oracle %v", name, source, tag, ok, want)
	}
	var got []regexDispatchHit
	r.Visit(name, func(source string, tag uint8) bool {
		if len(got) == 0 {
			// Visit must release the execution lock before invoking callbacks.
			r.Match(name)
		}
		got = append(got, regexDispatchHit{source, tag})
		return true
	})
	if !slices.Equal(got, want) {
		t.Fatalf("all original rules/tags for %q: got %v, oracle %v", name, got, want)
	}
	for _, limit := range []int{1, 2, 65} {
		got = nil
		r.Visit(name, func(source string, tag uint8) bool {
			got = append(got, regexDispatchHit{source, tag})
			return len(got) < limit
		})
		if !slices.Equal(got, want[:min(limit, len(want))]) {
			t.Fatalf("callback stop %d for %q: got %v, oracle %v", limit, name, got, want)
		}
	}
}

func assertRegexDispatchUnused(t *testing.T, r *Regex) {
	t.Helper()
	for i, batch := range r.batches {
		if batch.epoch != 0 || batch.builds != 0 {
			t.Fatalf("proven miss executed batch %d", i)
		}
	}
	for i, entry := range r.entries {
		if entry.re.machine.epoch != 0 || entry.re.machine.builds != 0 {
			t.Fatalf("proven miss executed individual rule %d", i)
		}
	}
}

func TestRegexDispatchProvenMissesDoNotExecute(t *testing.T) {
	for _, literal := range []string{"x", "aaaaab"} {
		t.Run(literal, func(t *testing.T) {
			var sources []string
			for i := range 130 {
				sources = append(sources, fmt.Sprintf(`^%s[cd]{%d}$`, literal, i+1))
			}
			r, oracles := buildRegexDispatchOracle(t, sources)
			// "aaaa" is an index hit for some "aaaaab" rules, but the
			// complete required literal independently proves every miss.
			assertRegexDispatchOracle(t, r, oracles, strings.Repeat("a", 20))
			assertRegexDispatchUnused(t, r)
		})
	}
}

func TestRegexDispatchExcludesBeforeEscalation(t *testing.T) {
	for _, literal := range []string{"x", "aaaaab"} {
		t.Run(literal, func(t *testing.T) {
			var sources []string
			for i := range 130 {
				sources = append(sources, fmt.Sprintf(`^%s[cd]{%d}$`, literal, i+1))
			}
			sources = append(sources, `^aaaa`)
			r, oracles := buildRegexDispatchOracle(t, sources)
			const name = "aaaa.test"
			source, tag, ok := r.Match(name)
			want := oracles[len(oracles)-1]
			if !want.re.MatchString(name) || !ok || (regexDispatchHit{source, tag}) != want.hit {
				t.Fatalf("eligible original rule lost after excluded candidates: %q %d %v", source, tag, ok)
			}
			for i, batch := range r.batches {
				if batch.epoch != 0 || batch.builds != 0 {
					t.Fatalf("excluded candidates escalated into batch %d", i)
				}
			}
			if r.entries[len(r.entries)-1].re.machine.epoch == 0 {
				t.Fatal("eligible sparse rule did not execute")
			}
			assertRegexDispatchOracle(t, r, oracles, name)
		})
	}
}

func TestRegexDispatchTrueNoLiteralExecutes(t *testing.T) {
	var sources []string
	for i := range 70 {
		sources = append(sources, fmt.Sprintf(`^[a-z]{%d}$`, i+1))
	}
	r, oracles := buildRegexDispatchOracle(t, sources)
	for _, entry := range r.entries {
		if entry.lit != "" {
			t.Fatal("fixture has a required literal")
		}
	}
	for _, name := range []string{strings.Repeat("a", 65), "", "123"} {
		assertRegexDispatchOracle(t, r, oracles, name)
	}
	for i, batch := range r.batches {
		if batch.epoch == 0 {
			t.Fatalf("genuine no-literal batch %d never executed", i)
		}
	}
}

func TestRegexDispatchUnicodeAndUppercaseBypass(t *testing.T) {
	var sources []string
	for i := range 70 {
		sources = append(sources, `(?i)k`+strings.Repeat("()", i))
	}
	r, oracles := buildRegexDispatchOracle(t, sources)
	for _, name := range []string{"K", "K", "\xffK", "k", "ſ", ""} {
		assertRegexDispatchOracle(t, r, oracles, name)
	}
}

func TestRegexDispatchMixedOrderOracleAndSaturation(t *testing.T) {
	var sources []string
	for i := range 160 {
		source := []string{`z`, `aaaa`, `[a-z]{1,3}`, `aaaaab`, `(?i)k`}[i%5]
		sources = append(sources, source+strings.Repeat("()", i))
	}
	for _, cache := range []int{512, 64 << 10} {
		t.Run(fmt.Sprint(cache), func(t *testing.T) {
			r, oracles := buildRegexDispatchOracle(t, sources)
			// Use the same original programs/membership with a smaller cache
			// to check that saturation cannot alter first ID or Visit order.
			for i := range r.entries {
				machine, err := newAutomaton([]*syntax.Prog{r.entries[i].re.program}, cache)
				if err != nil {
					t.Fatal(err)
				}
				r.entries[i].re.machine = machine
			}
			for i := range r.batches {
				var programs []*syntax.Prog
				for _, id := range r.batchIDs[i] {
					programs = append(programs, r.entries[id].re.program)
				}
				machine, err := newAutomaton(programs, cache)
				if err != nil {
					t.Fatal(err)
				}
				r.batches[i] = machine
			}
			names := []string{"", "aaaa-z.example", strings.Repeat("a", 128), "aaaaab", "ABC", "K", "ſ", "\xffaaaa"}
			for i := range 32 {
				names = append(names, fmt.Sprintf("a%03dz", i))
			}
			for _, name := range names {
				assertRegexDispatchOracle(t, r, oracles, name)
			}
			if cache == 512 {
				for i, batch := range r.batches {
					if !batch.frozen {
						t.Fatalf("tiny cache in batch %d did not saturate", i)
					}
				}
			}
		})
	}
}

func TestRegexGroupingSkipsExcludedIndexedBatches(t *testing.T) {
	var sources []string
	for i := range 70 {
		sources = append(sources, fmt.Sprintf(`^blocked%d[.]example$`, i), fmt.Sprintf(`^[a-z]{%d}$`, i+1))
	}
	r, oracles := buildRegexDispatchOracle(t, sources)
	if r.unindexedBatches != 2 || len(r.batches) != 4 {
		t.Fatal("fixture did not retain both partial partition batches")
	}
	assertRegexDispatchOracle(t, r, oracles, "safe.test")
	for i, batch := range r.batches {
		if i < r.unindexedBatches && batch.epoch == 0 {
			t.Fatal("literal-free group did not execute")
		}
		if i >= r.unindexedBatches && (batch.epoch != 0 || batch.builds != 0) {
			t.Fatal("independently excluded indexed group executed")
		}
	}
}

func TestRegexGroupingOriginalIDMapsAndVisitOrder(t *testing.T) {
	for _, firstIndexed := range []bool{false, true} {
		t.Run(fmt.Sprint(firstIndexed), func(t *testing.T) {
			sources := []string{`^a`, `zzzz$`}
			if firstIndexed {
				sources[0], sources[1] = sources[1], sources[0]
			}
			for i := range 70 {
				sources = append(sources, `[a-z]`+strings.Repeat("()", i))
			}
			r, oracles := buildRegexDispatchOracle(t, sources)
			// The first indexed rule can match later in the name while its
			// original ID still precedes the unindexed rule's earlier match.
			for _, name := range []string{"aaaa.zzzz", "AAAA.ZZZZ", "Kaaaa.zzzz", "\xffaaaa.zzzz", ""} {
				assertRegexDispatchOracle(t, r, oracles, name)
			}
			if source, _, ok := r.Match("aaaa.zzzz"); !ok || source != sources[0] {
				t.Fatal("physical partition order displaced the earliest original rule")
			}
		})
	}
}

func TestRegexGroupingBudgetMetadataAndPartialBatch(t *testing.T) {
	var sources []string
	for i := range 64 {
		if i%2 == 0 {
			sources = append(sources, fmt.Sprintf(`^[a-z]{%d}$`, i+1))
		} else {
			sources = append(sources, fmt.Sprintf(`^indexed%d[.]example$`, i))
		}
	}
	build := func(limit int64) (*Regex, *ProgramBudget, error) {
		budget := NewProgramBudget(limit)
		builder := RegexBuilder{Budget: budget}
		for _, source := range sources {
			if err := builder.Add(source, 3); err != nil {
				return nil, budget, err
			}
		}
		r, err := builder.BuildChecked()
		return r, budget, err
	}
	r, budget, err := build(DefaultProgramBytes)
	if err != nil {
		t.Fatal(err)
	}
	if r.unindexedBatches != 1 || len(r.batches) != 2 {
		t.Fatal("one old contiguous batch did not become two charged partial batches")
	}
	metadata := int(unsafe.Sizeof(r.batches) + unsafe.Sizeof(r.batchIDs) + unsafe.Sizeof(r.unindexedBatches) + unsafe.Sizeof(r.batchBytes))
	metadata += cap(r.batches)*int(unsafe.Sizeof((*automaton)(nil))) + cap(r.batchIDs)*int(unsafe.Sizeof([]int32{}))
	seen := make([]bool, len(sources))
	for _, ids := range r.batchIDs {
		metadata += cap(ids) * int(unsafe.Sizeof(int32(0)))
		if len(ids) != 32 || cap(ids) != len(ids) {
			t.Fatal("partial-batch mapping retained hidden capacity")
		}
		last := int32(-1)
		for _, id := range ids {
			if id <= last || id < 0 || int(id) >= len(seen) || seen[id] {
				t.Fatal("original ID was dropped, duplicated or reordered")
			}
			last, seen[id] = id, true
		}
	}
	for _, found := range seen {
		if !found {
			t.Fatal("original ID missing from mapping")
		}
	}
	if metadata != r.batchBytes || budget.Used() != int64(r.ProgramBytes()) || r.Bytes() < r.ProgramBytes() {
		t.Fatal("group metadata or extra graph capacity escaped reservation")
	}
	exact, exactBudget, err := build(budget.Used())
	if err != nil || exact == nil || exactBudget.Used() != budget.Used() {
		t.Fatal("exact retained-capacity boundary refused", err)
	}
	if partial, _, err := build(budget.Used() - 1); partial != nil || !errors.Is(err, ErrProgramBudget) {
		t.Fatal("one-byte-short grouping bound became a partial matcher", err)
	}
}

type regexIndexedLaneCacheMode struct {
	name     string
	bytes    int
	saturate bool
}

func regexIndexedLaneCacheModes() []regexIndexedLaneCacheMode {
	return []regexIndexedLaneCacheMode{
		{name: "normal", bytes: 64 << 10},
		{name: "disabled", bytes: 0},
		{name: "tiny", bytes: 512},
		{name: "saturated", bytes: 512, saturate: true},
	}
}

// These owned fixtures retain original programs and group IDs. Warm actual
// transitions for the saturated mode; never force a frozen flag or replay an
// original performance corpus with a different workload.
func setRegexIndexedLaneCaches(t *testing.T, r *Regex, mode regexIndexedLaneCacheMode) {
	t.Helper()
	var machines []*automaton
	for i := range r.entries {
		machine, err := newAutomaton([]*syntax.Prog{r.entries[i].re.program}, mode.bytes)
		if err != nil {
			t.Fatal(err)
		}
		r.entries[i].re.machine = machine
		machines = append(machines, machine)
	}
	for i, ids := range r.batchIDs {
		var programs []*syntax.Prog
		for _, id := range ids {
			programs = append(programs, r.entries[id].re.program)
		}
		machine, err := newAutomaton(programs, mode.bytes)
		if err != nil {
			t.Fatal(err)
		}
		r.batches[i] = machine
		machines = append(machines, machine)
	}
	if mode.saturate {
		for _, machine := range machines {
			for _, name := range []string{"", "a", "0", ".", "\n", "aa", "a.", ".a", "a\n", "\na", ".\n", "\n.", "zzzzc", "zzzzdd", "zzzz" + strings.Repeat("c", 128)} {
				machine.matchMask(name)
			}
		}
		// The first full group has many simultaneously active successors on
		// "a" or "z". Its real 512-byte cache cannot retain that set.
		if len(r.batchIDs) == 0 || len(r.batchIDs[0]) != automatonRules || !r.batches[0].frozen || len(r.batches[0].states) == 0 {
			t.Fatal("saturated fixture did not reach a real enabled-cache refusal")
		}
	}
}

func regexIndexedLaneSources(indexed, unindexed bool) []string {
	var sources []string
	if unindexed {
		// Include a zero-width language and real empty required literals.
		sources = append(sources, `(?:)`)
		for i := range 69 {
			sources = append(sources, `[a-z]+`+strings.Repeat("()", i))
		}
	}
	if indexed {
		for i := range 70 {
			sources = append(sources, fmt.Sprintf(`^zzzz[cd]{%d}$`, i+1))
		}
	}
	return sources
}

func TestRegexIndexedLaneAbsenceOracleAndCachePaths(t *testing.T) {
	for _, kind := range []struct {
		name               string
		indexed, unindexed bool
	}{
		{name: "mixed", indexed: true, unindexed: true},
		{name: "indexed-only", indexed: true},
		{name: "unindexed-only", unindexed: true},
	} {
		for _, mode := range regexIndexedLaneCacheModes() {
			t.Run(kind.name+"/"+mode.name, func(t *testing.T) {
				r, oracles := buildRegexDispatchOracle(t, regexIndexedLaneSources(kind.indexed, kind.unindexed))
				setRegexIndexedLaneCaches(t, r, mode)
				programBytes, retainedBytes := r.ProgramBytes(), r.Bytes()
				for _, name := range []string{"", "a", "abc", "safe.test", strings.Repeat("a", 65)} {
					if r.hasIndexedGram(name) {
						t.Fatalf("fixture unexpectedly has a selected gram for %q", name)
					}
					assertRegexDispatchOracle(t, r, oracles, name)
					source, tag, ok := r.scan(name) // preserve the independently classified wrapper
					wantSource, wantTag, wantOK := r.Match(name)
					if source != wantSource || tag != wantTag || ok != wantOK {
						t.Fatal("scan wrapper lost the original first rule")
					}
				}
				if !mode.saturate {
					for i := r.unindexedBatches; i < len(r.batches); i++ {
						if r.batches[i].epoch != 0 || r.batches[i].builds != 0 {
							t.Fatal("absent indexed lane executed")
						}
					}
					if kind.unindexed && r.batches[0].epoch == 0 {
						t.Fatal("real empty-literal lane was skipped")
					}
				}
				if r.ProgramBytes() != programBytes || r.Bytes() != retainedBytes {
					t.Fatal("query pruning changed retained accounting")
				}
			})
		}
	}
}

func TestRegexIndexedLaneBitmapCollisionAndPresentGram(t *testing.T) {
	for _, mode := range regexIndexedLaneCacheModes() {
		t.Run(mode.name, func(t *testing.T) {
			sources := regexIndexedLaneSources(false, true)
			for i := range 70 {
				sources = append(sources, fmt.Sprintf(`^aaaaab[cd]{%d}$`, i+1))
			}
			r, oracles := buildRegexDispatchOracle(t, sources)
			setRegexIndexedLaneCaches(t, r, mode)
			// A superset bitmap models arbitrary bit collisions. Exact map
			// lookup must still prove absence; a positive bit alone cannot.
			for i := range r.bits {
				r.bits[i] = ^uint64(0)
			}
			if r.hasIndexedGram("safe.test") {
				t.Fatal("bitmap collision became an actual gram hit")
			}
			assertRegexDispatchOracle(t, r, oracles, "safe.test")
			// The first indexed rule selects "aaaa", which is present even
			// though its complete required literal "aaaaab" is absent.
			if !r.hasIndexedGram("aaaa.test") {
				t.Fatal("actual gram key was treated as absent")
			}
			assertRegexDispatchOracle(t, r, oracles, "aaaa.test")
			// Presence is only eligibility. A real indexed hit must retain
			// source/tag and Visit ordering through the complete old lane.
			if !r.hasIndexedGram("aaaaabc") {
				t.Fatal("indexed hit lost its necessary selected gram")
			}
			assertRegexDispatchOracle(t, r, oracles, "aaaaabc")
		})
	}
}

func TestRegexIndexedLaneOrderTagsAndReentrancy(t *testing.T) {
	for _, indexedFirst := range []bool{false, true} {
		for _, mode := range regexIndexedLaneCacheModes() {
			t.Run(fmt.Sprint(indexedFirst)+"/"+mode.name, func(t *testing.T) {
				var sources []string
				for i := range 70 {
					unindexed := `[a-z]+` + strings.Repeat("()", i)
					indexed := `zzzz$` + strings.Repeat("()", i)
					if indexedFirst {
						sources = append(sources, indexed, unindexed)
					} else {
						sources = append(sources, unindexed, indexed)
					}
				}
				r, oracles := buildRegexDispatchOracle(t, sources)
				setRegexIndexedLaneCaches(t, r, mode)
				for _, name := range []string{"aaaa", "aaaa.zzzz", "zzzz", "", "AAAA.ZZZZ", "Kaaaa.zzzz", "\xffaaaa.zzzz"} {
					// Existing independent helper checks the earliest original
					// rule/tag, every Visit hit, callback stops1/2/65 and a
					// reentrant Match call from the first Visit callback.
					assertRegexDispatchOracle(t, r, oracles, name)
				}
				var expected []regexDispatchHit
				for _, oracle := range oracles {
					if oracle.re.MatchString("aaaa.zzzz") {
						expected = append(expected, oracle.hit)
					}
				}
				var nested []regexDispatchHit
				called := false
				r.Visit("aaaa.zzzz", func(string, uint8) bool {
					if !called {
						called = true
						r.Visit("aaaa.zzzz", func(source string, tag uint8) bool {
							nested = append(nested, regexDispatchHit{source, tag})
							return true
						})
					}
					return false
				})
				if !called || !slices.Equal(nested, expected) {
					t.Fatal("reentrant Visit lost ordered original source/tags")
				}
			})
		}
	}
}

func TestRegexIndexedLaneUnsafeBypass(t *testing.T) {
	sources := []string{`(?i)kkkk`, `(?i)ssss`}
	sources = append(sources, regexIndexedLaneSources(true, true)...)
	for _, mode := range regexIndexedLaneCacheModes() {
		for _, name := range []string{"KKKK", "KKKK", "ſſſſ", "\xffKKKK", "SAFE.TEST"} {
			t.Run(mode.name+"/"+fmt.Sprintf("%x", name), func(t *testing.T) {
				r, oracles := buildRegexDispatchOracle(t, sources)
				setRegexIndexedLaneCaches(t, r, mode)
				if prefilterSafe(name) {
					t.Fatal("unsafe fixture incorrectly classified safe")
				}
				assertRegexDispatchOracle(t, r, oracles, name)
				if !mode.saturate {
					for i := r.unindexedBatches; i < len(r.batches); i++ {
						if r.batches[i].epoch == 0 {
							t.Fatal("unsafe Visit pruned an indexed group")
						}
					}
				}
			})
		}
	}
}

func TestRegexIndexedLaneZeroAllocation(t *testing.T) {
	for _, mode := range regexIndexedLaneCacheModes() {
		t.Run(mode.name, func(t *testing.T) {
			r, oracles := buildRegexDispatchOracle(t, regexIndexedLaneSources(true, true))
			setRegexIndexedLaneCaches(t, r, mode)
			for _, name := range []string{"aaaa", "zzzzc", "AAAA", "\xffaaaa"} {
				assertRegexDispatchOracle(t, r, oracles, name)
				wantCount := 0
				var wantFirst regexDispatchHit
				for _, oracle := range oracles {
					if oracle.re.MatchString(name) {
						if wantCount == 0 {
							wantFirst = oracle.hit
						}
						wantCount++
					}
				}
				var source string
				var tag uint8
				var matched bool
				seen := 0
				visit := func(string, uint8) bool { seen++; return true }
				allocations := testing.AllocsPerRun(100, func() {
					source, tag, matched = r.Match(name)
					seen = 0
					r.Visit(name, visit)
				})
				if allocations != 0 {
					t.Fatalf("%s/%q: %g query allocations", mode.name, name, allocations)
				}
				if matched != (wantCount != 0) || matched && (regexDispatchHit{source, tag}) != wantFirst || seen != wantCount {
					t.Fatal("allocation fixture lost the independently expected result")
				}
			}
		})
	}
}
