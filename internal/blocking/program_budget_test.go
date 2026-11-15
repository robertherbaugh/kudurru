package blocking

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestProgramBudgetRefusesWholeListAndReuse(t *testing.T) {
	for _, rule := range []string{`/a.*z/`, `/a.*z/$important`, `/a.*z/$badfilter`} {
		spec := Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("original", parser.FormatAdblock, []string{"before.test", rule, "after.test"})}}
		if got, err := Compile(spec, nil, CompileOptions{ProgramBytes: 100}); got != nil || !errors.Is(err, matcher.ErrProgramBudget) {
			t.Fatalf("%q: partial=%v err=%v", rule, got != nil, err)
		}
	}
	spec := Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("original", parser.FormatRegex, []string{"a.*z"})}}
	first, err := Compile(spec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ProgramBytes() == 0 || int64(first.Stats().Bytes) < first.ProgramBytes() {
		t.Fatal("unaccounted program capacity")
	}
	if reused, err := Compile(spec, first, CompileOptions{}); err != nil || reused != first {
		t.Fatal("unchanged program not reused", err)
	}
	if reused, err := Compile(spec, first, CompileOptions{ProgramBytes: 128 << 20}); err != nil || reused != first {
		t.Fatal("default list budget differs from explicit 128 MiB", err)
	}
	if got, err := Compile(spec, first, CompileOptions{ProgramBytes: 100}); got != nil || !errors.Is(err, matcher.ErrProgramBudget) {
		t.Fatal("budget change reused oversized list")
	}
	if got, err := Compile(spec, first, CompileOptions{ProgramBudget: matcher.NewProgramBudget(100)}); got != nil || !errors.Is(err, matcher.ErrProgramBudget) {
		t.Fatal("reuse bypassed candidate reservation")
	}
	changed, err := Compile(spec, first, CompileOptions{ProgramBytes: 64 << 20})
	if err != nil || changed == first || changed.Fingerprint() == first.Fingerprint() {
		t.Fatal("resource policy missing from fingerprint", err)
	}
}

func TestProgramBudgetReuseAncestorReservations(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		format   parser.Format
		rule     string
		wantRule string
	}{
		{"zero-bytes", parser.FormatDomains, "owned.test", "owned.test"},
		{"program-bytes", parser.FormatRegex, `^owned[.]test$`, `/^owned[.]test$/`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			spec := Spec{Name: "owned", Mode: ModeBlock, Sources: []Source{LinesSource("captured", fixture.format, []string{fixture.rule})}}
			owner, err := Compile(spec, nil, CompileOptions{})
			if err != nil || owner == nil {
				t.Fatalf("baseline failed: list=%t err=%v", owner != nil, err)
			}
			bytes := owner.ProgramBytes()
			if stats := owner.Stats(); stats.Entries != 1 || stats.Invalid != 0 || stats.Unsupported != 0 || (bytes == 0) != (fixture.name == "zero-bytes") {
				t.Fatalf("baseline cannot exercise reservation: bytes=%d stats=%+v", bytes, stats)
			}
			fingerprint := owner.Fingerprint()
			capacity := 2*max(bytes, 1) + 4
			for _, tc := range []struct {
				name        string
				limits      [3]int64
				prefill     [3]int64
				nilParent   bool
				wantRefusal bool
			}{
				{name: "nil-parent", nilParent: true},
				{name: "exact-three-level-chain", limits: [3]int64{bytes, bytes, bytes}},
				{name: "zero-chain", limits: [3]int64{}, wantRefusal: bytes > 0},
				{name: "negative-parent", limits: [3]int64{capacity, capacity, -1}, wantRefusal: true},
				{name: "negative-middle", limits: [3]int64{capacity, -1, capacity}, wantRefusal: true},
				{name: "negative-root", limits: [3]int64{-1, capacity, capacity}, wantRefusal: true},
				{name: "short-parent", limits: [3]int64{capacity, capacity, bytes - 1}, wantRefusal: true},
				{name: "short-middle", limits: [3]int64{capacity, bytes - 1, capacity}, wantRefusal: true},
				{name: "short-root", limits: [3]int64{bytes - 1, capacity, capacity}, wantRefusal: true},
				{name: "exhausted-parent", limits: [3]int64{capacity, capacity, capacity}, prefill: [3]int64{0, 0, capacity}, wantRefusal: bytes > 0},
				{name: "exhausted-middle", limits: [3]int64{capacity, capacity, capacity}, prefill: [3]int64{0, capacity, 0}, wantRefusal: bytes > 0},
				{name: "exhausted-root", limits: [3]int64{capacity, capacity, capacity}, prefill: [3]int64{capacity, 0, 0}, wantRefusal: bytes > 0},
				{name: "prefilled-exact-root", limits: [3]int64{capacity + bytes, capacity, capacity}, prefill: [3]int64{capacity, 0, 0}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var parent *matcher.ProgramBudget
					var ancestors [3]*matcher.ProgramBudget
					var before [3]int64
					if !tc.nilParent {
						ancestors[0] = matcher.NewProgramBudget(tc.limits[0])
						ancestors[1] = ancestors[0].Child(tc.limits[1])
						ancestors[2] = ancestors[1].Child(tc.limits[2])
						parent = ancestors[2]
						for i, amount := range tc.prefill {
							if amount != 0 {
								if err := ancestors[i].Reserve(amount); err != nil {
									t.Fatal("invalid prefill fixture", err)
								}
							}
						}
						for i, ancestor := range ancestors {
							before[i] = ancestor.Used()
						}
					}
					reused, err := Compile(spec, owner, CompileOptions{ProgramBudget: parent})
					if tc.wantRefusal {
						if reused != nil || !errors.Is(err, matcher.ErrProgramBudget) {
							t.Fatalf("reservation refusal lost: list=%t err=%v", reused != nil, err)
						}
					} else if err != nil || reused != owner {
						t.Fatal("reservation changed reuse identity", err)
					}
					for i, ancestor := range ancestors {
						want := before[i]
						if ancestor != nil && !tc.wantRefusal {
							want += bytes
						}
						if ancestor.Used() != want {
							t.Fatalf("ancestor %d reservation=%d want=%d", i, ancestor.Used(), want)
						}
					}
				})
			}
			t.Run("siblings-share-exact-root", func(t *testing.T) {
				root := matcher.NewProgramBudget(2 * bytes)
				for i := range 3 {
					sibling := root.Child(bytes)
					before := root.Used()
					reused, err := Compile(spec, owner, CompileOptions{ProgramBudget: sibling})
					if i == 2 && bytes > 0 {
						if reused != nil || !errors.Is(err, matcher.ErrProgramBudget) || sibling.Used() != 0 || root.Used() != before {
							t.Fatal("sibling refusal changed candidate reservation", err)
						}
					} else if err != nil || reused != owner || sibling.Used() != bytes || root.Used() != before+bytes {
						t.Fatal("sibling did not charge exact shared reservation", err)
					}
				}
			})
			match, ok := owner.Match("owned.test")
			if !ok || match.Rule() != fixture.wantRule || owner.ProgramBytes() != bytes || owner.Fingerprint() != fingerprint || owner.Sources()[0].Name != "captured" {
				t.Fatal("reuse/refusal changed original owner or rule identity")
			}
		})
	}
}

func TestProgramBudgetReuseRetainedByteGuards(t *testing.T) {
	spec := Spec{Name: "owned", Mode: ModeBlock, Sources: []Source{LinesSource("captured", parser.FormatRegex, []string{`^owned[.]test$`})}}
	for _, limit := range []int64{128 << 20, 1 << 20} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			owner, err := Compile(spec, nil, CompileOptions{ProgramBytes: limit})
			if err != nil || owner == nil || owner.ProgramBytes() <= 0 || owner.ProgramBytes() > limit {
				t.Fatalf("baseline failed: list=%t err=%v", owner != nil, err)
			}
			bytes, fingerprint := owner.ProgramBytes(), owner.Fingerprint()
			for _, invalid := range []int64{-1, limit + 1} {
				for _, nilParent := range []bool{false, true} {
					t.Run(fmt.Sprintf("retained-%d/nil-%t", invalid, nilParent), func(t *testing.T) {
						root := matcher.NewProgramBudget(128 << 20)
						if err := root.Reserve(7); err != nil {
							t.Fatal(err)
						}
						parent := root.Child(128 << 20)
						if nilParent {
							parent = nil
						}
						// An invalid stored reservation must not exploit nil-parent
						// admission or a parent larger than the explicit list bound.
						invalidOwner := *owner
						invalidOwner.programBytes = invalid
						reused, err := Compile(spec, &invalidOwner, CompileOptions{ProgramBytes: limit, ProgramBudget: parent})
						if reused != nil || !errors.Is(err, matcher.ErrProgramBudget) || root.Used() != 7 || parent.Used() != 0 {
							t.Fatal("invalid retained reservation bypassed list guard or debited parent", err)
						}
					})
				}
			}
			if reused, err := Compile(spec, owner, CompileOptions{ProgramBytes: limit}); err != nil || reused != owner {
				t.Fatal("valid explicit list-bound reuse changed", err)
			}
			match, ok := owner.Match("owned.test")
			if !ok || match.Rule() != `/^owned[.]test$/` || owner.ProgramBytes() != bytes || owner.Fingerprint() != fingerprint {
				t.Fatal("invalid-owner fixture changed the original list")
			}
		})
	}
}

func TestProgramBudgetLazyBadfilterValidation(t *testing.T) {
	for _, line := range []string{"plain.test", "||plain.test^$dnstype=A"} {
		spec := Spec{Name: "fingerprint", Mode: ModeBlock, Sources: []Source{LinesSource("captured", parser.FormatAdblock, []string{line})}}
		if _, directives, err := fingerprint(&spec, -1, 100); err != nil || len(directives) != 0 {
			t.Fatalf("non-badfilter fingerprint compiled a program: line=%q directives=%v err=%v", line, directives, err)
		}
	}
	first, second := `/^gone[.]test$/`, `/^away[.]test$/`
	validation := matcher.NewProgramBudgetWithoutClassProof(1 << 20)
	if _, err := matcher.CompilePatternWithBudget(first, validation); err != nil {
		t.Fatal(err)
	}
	limit := validation.Used()
	if limit <= 0 {
		t.Fatal("validation fixture did not reserve a program")
	}
	if _, err := matcher.CompilePatternWithBudget(second, matcher.NewProgramBudgetWithoutClassProof(limit)); err != nil {
		t.Fatal("second validation fixture exceeds the first program's reserve", err)
	}
	large := "/" + strings.Repeat("a{100}", 20) + "/$badfilter"
	refused := Spec{Name: "refused", Mode: ModeBlock, Sources: []Source{LinesSource("captured", parser.FormatAdblock, []string{large})}}
	parent := matcher.NewProgramBudget(64)
	if err := parent.Reserve(7); err != nil {
		t.Fatal(err)
	}
	if partial, err := Compile(refused, nil, CompileOptions{ProgramBytes: limit, ProgramBudget: parent}); partial != nil || !errors.Is(err, matcher.ErrProgramBudget) || parent.Used() != 7 {
		t.Fatal("real badfilter refusal changed candidate reservation", err)
	}
	excluded := LinesSource("first", parser.FormatAdblock, []string{large, first, first + "$badfilter", "kept.test"})
	excluded.Exclude = []string{large}
	spec := Spec{Name: "directives", Mode: ModeBlock, Sources: []Source{excluded, LinesSource("second", parser.FormatAdblock, []string{second, second + "$badfilter"})}}
	// The build still needs temporary headroom to compile each directive
	// before discarding it; only its final retained reservation is zero.
	parent = matcher.NewProgramBudget(limit)
	opts := CompileOptions{ProgramBytes: limit, ProgramBudget: parent}
	owner, err := Compile(spec, nil, opts)
	if err != nil || owner == nil {
		t.Fatalf("excluded directive or sequential validation refused: list=%t err=%v", owner != nil, err)
	}
	if stats := owner.Stats(); stats.Entries != 1 || stats.Invalid != 0 || stats.Unsupported != 0 || owner.ProgramBytes() != 0 || parent.Used() != 0 {
		t.Fatalf("validation retained a program or lost rules: bytes=%d parent=%d stats=%+v", owner.ProgramBytes(), parent.Used(), stats)
	}
	if match, ok := owner.Match("kept.test"); !ok || match.Rule() != "kept.test" {
		t.Fatal("badfilter validation changed surviving rule identity")
	}
	for _, name := range []string{"gone.test", "away.test"} {
		if _, ok := owner.Match(name); ok {
			t.Fatal("sequential badfilter directives failed cancellation", name)
		}
	}
	zeroParent := matcher.NewProgramBudget(0)
	if reused, err := Compile(spec, owner, CompileOptions{ProgramBytes: limit, ProgramBudget: zeroParent}); err != nil || reused != owner || parent.Used() != 0 || zeroParent.Used() != 0 {
		t.Fatal("revalidated badfilters changed reuse identity or candidate bytes", err)
	}
}

func TestProgramBudgetDiscardedDuplicatesAndDiagnosticGate(t *testing.T) {
	base := "||ads.example^$dnstype=A\n"
	spec := Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{BytesSource("original", parser.FormatAdblock, []byte(base))}}
	one, err := Compile(spec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec.Sources[0] = BytesSource("original", parser.FormatAdblock, []byte(strings.Repeat(base, 100)))
	many, err := Compile(spec, nil, CompileOptions{ProgramBytes: one.ProgramBytes() + 1000})
	if err != nil || many.Stats().Entries != 1 || many.ProgramBytes() != one.ProgramBytes() {
		t.Fatalf("duplicate leaked reservation: %v", err)
	}
	// Optional excluded-rule evidence cannot invent a match when its own
	// program reserve is exhausted; the surviving exact rule still executes.
	large := "/" + strings.Repeat("a{1000}", 10) + "/"
	spec.Sources[0] = BytesSource("original", parser.FormatAdblock, []byte("safe.test\n"+large))
	spec.Sources[0].Exclude = []string{large}
	list, err := Compile(spec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !list.exclusions.truncated {
		t.Fatal("missing explicit diagnostic gate")
	}
	if _, ok := list.Match("safe.test"); !ok {
		t.Fatal("diagnostic failure changed serving")
	}
}

func TestProgramBudgetOriginalModifierWorkload(t *testing.T) {
	// Preserve BenchmarkModifiers' actual workload under the approved default
	// and an independently bounded whole-candidate parent.
	lines := make([]string, 10000)
	for i := range lines {
		lines[i] = fmt.Sprintf("||host%d.example^$client=192.0.2.0/24,dnstype=AAAA", i)
	}
	spec := Spec{Name: "original", Mode: ModeBlock, Sources: []Source{LinesSource("captured", parser.FormatAdblock, lines)}}
	parent := matcher.NewProgramBudget(128 << 20)
	opts := CompileOptions{ProgramBudget: parent}
	list, err := Compile(spec, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if list.Stats().Entries != len(lines) || list.Stats().Invalid != 0 || list.Stats().Unsupported != 0 {
		t.Fatal("original modifier workload lost rules", list.Stats())
	}
	if partial, err := Compile(spec, list, CompileOptions{ProgramBytes: 32 << 20}); partial != nil || !errors.Is(err, matcher.ErrProgramBudget) {
		t.Fatalf("smaller explicit list bound bypassed: partial=%v error=%v", partial != nil, err)
	}
	check := func() {
		for _, id := range []int{0, len(lines) - 1} {
			ctx := Context{Name: fmt.Sprintf("host%d.example", id), Type: dns.TypeAAAA, ClientIP: netip.MustParseAddr("192.0.2.5")}
			match, ok := list.MatchContext(ctx)
			if !ok || match.Rule() != lines[id] {
				t.Fatal("original rule identity was lost", id)
			}
			ctx.ClientIP = netip.MustParseAddr("192.0.3.5")
			if _, ok := list.MatchContext(ctx); ok {
				t.Fatal("program ownership bypassed client selectors")
			}
			ctx.ClientIP, ctx.Type = netip.MustParseAddr("192.0.2.5"), dns.TypeA
			if _, ok := list.MatchContext(ctx); ok {
				t.Fatal("program ownership bypassed question selectors")
			}
		}
	}
	check()
	reserved := parent.Used()
	if reserved != list.ProgramBytes() || reserved <= 0 || reserved > 128<<20 {
		t.Fatal("unexpected original-workload reservation", reserved)
	}
	// A second list cannot multiply the candidate's bound. Refusal rolls back
	// only its own reservations and preserves the first immutable owner.
	spec.Name = "second"
	second, err := Compile(spec, nil, opts)
	if reserved*2 > 128<<20 {
		if second != nil || !errors.Is(err, matcher.ErrProgramBudget) {
			t.Fatalf("partial=%v error=%v", second != nil, err)
		}
		if parent.Used() != reserved {
			t.Fatal("candidate refusal changed the original reservation")
		}
	} else if err != nil || second == nil || parent.Used() != reserved+second.ProgramBytes() || parent.Used() > 128<<20 {
		t.Fatal("candidate accounting changed on admitted second owner", err)
	}
	check()
	t.Logf("rules=%d reserved=%d candidate_budget=%d", len(lines), reserved, 128<<20)
}

func TestProgramBudgetGroupedRegexRefusalPreservesOwner(t *testing.T) {
	oldSpec := Spec{Name: "owned", Mode: ModeBlock, Sources: []Source{LinesSource("captured-old", parser.FormatRegex, []string{`^old[.]example$`})}}
	var rules []string
	for i := range 64 {
		if i%2 == 0 {
			rules = append(rules, fmt.Sprintf(`^[a-z]{%d}$`, i+1))
		} else {
			rules = append(rules, fmt.Sprintf(`^indexed%d[.]example$`, i))
		}
	}
	newSpec := Spec{Name: "owned", Mode: ModeBlock, Sources: []Source{LinesSource("captured-new", parser.FormatRegex, rules)}}
	oldSize, err := Compile(oldSpec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	newSize, err := Compile(newSpec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			limit := oldSize.ProgramBytes() + newSize.ProgramBytes()
			if short {
				limit--
			}
			parent := matcher.NewProgramBudget(limit)
			opts := CompileOptions{ProgramBudget: parent}
			old, err := Compile(oldSpec, nil, opts)
			if err != nil {
				t.Fatal(err)
			}
			reserved, fingerprint := parent.Used(), old.Fingerprint()
			next, err := Compile(newSpec, old, opts)
			if short {
				if next != nil || !errors.Is(err, matcher.ErrProgramBudget) || parent.Used() != reserved {
					t.Fatal("grouped candidate refusal leaked a partial owner or reservation", err)
				}
			} else if err != nil || next == nil || next.Stats().Entries != len(rules) || next.Stats().Invalid != 0 || next.Stats().Unsupported != 0 || parent.Used() != limit {
				t.Fatal("exact grouped-candidate reservation lost rules or accounting", err)
			}
			match, ok := old.Match("old.example")
			if !ok || match.Rule() != `/^old[.]example$/` || old.Fingerprint() != fingerprint || old.ProgramBytes() != reserved || old.Sources()[0].Name != "captured-old" {
				t.Fatal("grouping/refusal changed original owner or source provenance")
			}
		})
	}
}

func TestProgramBudgetASCIIProofCandidateIdentity(t *testing.T) {
	oldSpec := Spec{Name: "owned", Mode: ModeBlock, Sources: []Source{LinesSource("captured-old", parser.FormatRegex, []string{`^old[.]example$`})}}
	rules := []string{`/^ads[.]example$/`, `/^gone[.]example$/`, `/^gone[.]example$/$badfilter`, `/^client[.]example$/$client=192.0.2.5,dnstype=AAAA`}
	newSpec := Spec{Name: "owned", Mode: ModeBlock, Sources: []Source{LinesSource("captured-new", parser.FormatAdblock, rules)}}
	oldSize, err := Compile(oldSpec, nil, CompileOptions{})
	if err != nil || oldSize == nil {
		t.Fatalf("old baseline failed: list=%t err=%v", oldSize != nil, err)
	}
	if stats := oldSize.Stats(); stats.Entries != 1 || stats.Invalid != 0 || stats.Unsupported != 0 || stats.RegexDropped != 0 || oldSize.ProgramBytes() <= 0 {
		t.Fatalf("old baseline cannot size candidate: stats=%+v bytes=%d", stats, oldSize.ProgramBytes())
	}
	newSize, err := Compile(newSpec, nil, CompileOptions{})
	if err != nil || newSize == nil {
		t.Fatalf("new baseline failed: list=%t err=%v", newSize != nil, err)
	}
	if stats := newSize.Stats(); stats.Entries != 2 || stats.Invalid != 0 || stats.Unsupported != 0 || stats.RegexDropped != 0 || newSize.ProgramBytes() <= 0 {
		t.Fatalf("new baseline cannot size candidate: stats=%+v bytes=%d", stats, newSize.ProgramBytes())
	}
	for _, noProof := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			t.Run(fmt.Sprintf("identity-%t/short-%t", noProof, short), func(t *testing.T) {
				limit := oldSize.ProgramBytes() + newSize.ProgramBytes()
				if short {
					limit--
				}
				parent := matcher.NewProgramBudget(limit)
				if noProof {
					parent = matcher.NewProgramBudgetWithoutClassProof(limit)
				}
				opts := CompileOptions{ProgramBudget: parent}
				old, err := Compile(oldSpec, nil, opts)
				if err != nil {
					t.Fatal(err)
				}
				reserved, fingerprint := parent.Used(), old.Fingerprint()
				next, err := Compile(newSpec, old, opts)
				if short {
					if next != nil || !errors.Is(err, matcher.ErrProgramBudget) || parent.Used() != reserved {
						t.Fatal("retained-byte refusal changed original candidate", err)
					}
				} else {
					if err != nil || next == nil || next.Stats().Invalid != 0 || next.Stats().Unsupported != 0 || parent.Used() != limit {
						t.Fatal("optional proof shortage lost rules or byte accounting", err)
					}
					if match, ok := next.Match("ads.example"); !ok || match.Rule() != rules[0] {
						t.Fatal("lost original rule identity")
					}
					if _, ok := next.Match("gone.example"); ok {
						t.Fatal("proof-disabled badfilter validation changed the decision")
					}
					ctx := Context{Name: "client.example", Type: dns.TypeAAAA, ClientIP: netip.MustParseAddr("192.0.2.5")}
					if match, ok := next.MatchContext(ctx); !ok || match.Rule() != rules[3] {
						t.Fatal("client/question rule identity lost")
					}
					ctx.ClientIP = netip.MustParseAddr("192.0.3.5")
					if _, ok := next.MatchContext(ctx); ok {
						t.Fatal("identity fallback bypassed original client predicate")
					}
					ctx.ClientIP, ctx.Type = netip.MustParseAddr("192.0.2.5"), dns.TypeA
					if _, ok := next.MatchContext(ctx); ok {
						t.Fatal("identity fallback bypassed original question predicate")
					}
					if reused, err := Compile(newSpec, next, CompileOptions{ProgramBudget: matcher.NewProgramBudgetWithoutClassProof(128 << 20)}); err != nil || reused != next {
						t.Fatal("optional proof policy changed original compile reuse identity", err)
					}
				}
				match, ok := old.Match("old.example")
				if !ok || match.Rule() != `/^old[.]example$/` || old.Fingerprint() != fingerprint || old.ProgramBytes() != reserved || old.Sources()[0].Name != "captured-old" {
					t.Fatal("constructor fallback/refusal changed captured original owner")
				}
			})
		}
	}
}
