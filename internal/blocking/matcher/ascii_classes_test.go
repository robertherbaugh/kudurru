package matcher

import (
	"context"
	"errors"
	"fmt"
	"regexp/syntax"
	"strings"
	"testing"
	"unsafe"
)

func asciiIdentity(a *automaton) bool {
	for symbol, class := range a.asciiClasses {
		if class != uint8(symbol) {
			return false
		}
	}
	return true
}

func asciiClassCount(a *automaton) int {
	var seen [128]bool
	count := 0
	for _, class := range a.asciiClasses {
		if !seen[class] {
			seen[class] = true
			count++
		}
	}
	return count
}

func consumingASCII(in *syntax.Inst, r rune) (bool, bool) {
	switch in.Op {
	case syntax.InstRune, syntax.InstRune1:
		return in.MatchRune(r), true
	case syntax.InstRuneAny:
		return true, true
	case syntax.InstRuneAnyNotNL:
		return r != '\n', true
	default:
		return false, false
	}
}

func TestAutomatonASCIIClassesExactPredicates(t *testing.T) {
	groups := [][]string{
		{`^[a-z]+[.]$`, `^[0-9]+$`, `\bword\B`, `(?m)^$`},
		{`(?i)k`, `(?i)s`, `(?i:Ab)(?-i:C)`, `[^a-z]`, `\w\W\d`},
		{`.`, `(?s:.)`, `\p{Greek}`, `\P{Latin}`, `\x00`, `\A\z`},
	}
	for id, expressions := range groups {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			a, oracles := oracleMachine(t, expressions, 65536)
			if asciiClassCount(a) == 128 {
				t.Fatal("small fixture unexpectedly declined exact proof")
			}
			for x := range 128 {
				for y := range x {
					if a.asciiClasses[x] != a.asciiClasses[y] {
						continue
					}
					if boundaryRune(rune(x)) != boundaryRune(rune(y)) {
						t.Fatal("merged boundary categories", x, y)
					}
					for i := range a.inst {
						left, consuming := consumingASCII(&a.inst[i], rune(x))
						right, _ := consumingASCII(&a.inst[i], rune(y))
						if consuming && left != right {
							t.Fatal("merged different actual instruction predicates", i, x, y)
						}
					}
				}
			}
			for range 2 {
				for symbol := range 128 {
					for _, name := range []string{string(rune(symbol)), "word" + string(rune(symbol)), string(rune(symbol)) + "a.\n"} {
						if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
							t.Fatalf("tagged ASCII parity %q: %b != %b", name, got, want)
						}
					}
				}
			}
		})
	}
}

func TestAutomatonASCIITransitionReuseAndEOF(t *testing.T) {
	// Retain the old full-byte ^$ language fixture. Exact classing should now
	// reuse its boundary categories instead of exhausting raw-byte keys.
	empty, emptyOracles := oracleMachine(t, []string{`^$`}, 512)
	for range 2 {
		for symbol := range 128 {
			name := string(rune(symbol))
			if got, want := empty.matchMask(name), oracleMask(emptyOracles, name); got != want {
				t.Fatal("original empty-language fixture changed", symbol)
			}
		}
		if empty.matchMask("") != 1 {
			t.Fatal("empty input lost EOF acceptance")
		}
	}
	if empty.frozen || asciiClassCount(empty) != 3 {
		t.Fatal("boundary-only fixture failed to reuse exact classes")
	}
	folded, _ := oracleMachine(t, []string{`(?i)k`, `(?i)s`}, 65536)
	if folded.asciiClasses['k'] != folded.asciiClasses['K'] || folded.asciiClasses['s'] != folded.asciiClasses['S'] {
		t.Fatal("actual folded ASCII predicates were not proved equal")
	}
	a, oracles := oracleMachine(t, []string{`^[a-z]+[.]$`, `^$`, `^\x00$`}, 65536)
	if a.asciiClasses['a'] != a.asciiClasses['b'] {
		t.Fatal("actual equal letter predicates were not compacted")
	}
	for _, pair := range [][2]byte{{'\n', 'a'}, {'a', '.'}, {'\n', '.'}} {
		if a.asciiClasses[pair[0]] == a.asciiClasses[pair[1]] {
			t.Fatal("boundary distinction lost", pair)
		}
	}
	if got := a.matchMask("aaa."); got != 1 {
		t.Fatal("wrong original tag", got)
	}
	builds := a.builds
	if got := a.matchMask("bbb."); got != 1 || a.builds != builds {
		t.Fatal("equivalent letters failed to reuse the exact action", got, builds, a.builds)
	}
	for range 2 {
		for _, name := range []string{"", "\x00", "\n", "a.", "b.", "\x00a.", "a.\x00"} {
			if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
				t.Fatalf("NUL/EOF parity %q: %b != %b", name, got, want)
			}
		}
	}
	if uint16(a.asciiClasses[0]) == 128 {
		t.Fatal("ASCII NUL collided with EOF")
	}
}

func TestAutomatonASCIIProofWorkFallback(t *testing.T) {
	t.Run("large-rune-table", func(t *testing.T) {
		var expression strings.Builder
		expression.WriteString("^[")
		for i := range 4096 {
			fmt.Fprintf(&expression, `\x{%x}`, 0x1000+2*i)
		}
		expression.WriteString("]$")
		p, err := compileProgram(expression.String())
		if err != nil {
			t.Fatal(err)
		}
		budget := NewProgramBudget(128 << 20)
		a, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, budget)
		if err != nil || !asciiIdentity(a) {
			t.Fatal("oversized optional predicate work did not choose full identity", err)
		}
		if budget.classWorkUsed >= classProofMachineWorkLimit {
			t.Fatal("large Rune table was scanned before its bound was checked", budget.classWorkUsed)
		}
		_, oracles := oracleMachine(t, []string{expression.String()}, 0)
		for _, name := range []string{"", "a", "က", "ခ", string(rune(0x2ffe)), "\xff"} {
			if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
				t.Fatal("identity fallback changed large-class language", name)
			}
		}
	})
	t.Run("machine-exhaustion-after-partial-refinement", func(t *testing.T) {
		expression := "^" + strings.Repeat("(?i:k)", 2000) + "$"
		p, err := compileProgram(expression)
		if err != nil {
			t.Fatal(err)
		}
		budget := NewProgramBudget(128 << 20)
		a, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, budget)
		if err != nil || !asciiIdentity(a) {
			t.Fatal("partial proof survived machine work exhaustion", err)
		}
		if budget.classWorkUsed <= 384 || budget.classWorkUsed > classProofMachineWorkLimit {
			t.Fatal("machine work was not bounded", budget.classWorkUsed)
		}
		_, oracles := oracleMachine(t, []string{expression}, 0)
		for _, name := range []string{"", "k", strings.Repeat("k", 2000), strings.Repeat("K", 2000), strings.Repeat("k", 1999) + "\xff"} {
			if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
				t.Fatal("proof exhaustion changed folded language")
			}
		}
	})
	t.Run("fold-cycle-precharge", func(t *testing.T) {
		p, err := compileProgram(`(?i)k`)
		if err != nil {
			t.Fatal(err)
		}
		var folded *syntax.Inst
		for i := range p.Inst {
			in := &p.Inst[i]
			if len(in.Rune) == 1 && syntax.Flags(in.Arg)&syntax.FoldCase != 0 {
				folded = in
				break
			}
		}
		if folded == nil {
			t.Fatal("fixture did not retain actual folded instruction")
		}
		budget := NewProgramBudget(1 << 20)
		work := asciiProofWork{used: classProofMachineWorkLimit - 1, budget: budget}
		if _, ok := asciiPredicateCost(folded, &work); ok || work.used != classProofMachineWorkLimit-1 || budget.classWorkUsed != 0 {
			t.Fatal("fold cycle performed work before local admission")
		}
		work = asciiProofWork{budget: budget}
		if cost, ok := asciiPredicateCost(folded, &work); !ok || cost < 4 || work.used < 2 || work.used > classProofMachineWorkLimit || budget.classWorkUsed != work.used {
			t.Fatal("actual Unicode fold walk was not charged", cost, ok, work.used)
		}
	})
}

func TestAutomatonASCIIProofAncestorAtomicity(t *testing.T) {
	root := NewProgramBudget(128 << 20)
	child := root.Child(128 << 20)
	leaf := child.Child(128 << 20)
	root.classWorkUsed = classProofCandidateWorkLimit - 3
	if leaf.tryClassWork(4) || root.classWorkUsed != classProofCandidateWorkLimit-3 || child.classWorkUsed != 0 || leaf.classWorkUsed != 0 {
		t.Fatal("exhausted ancestor caused partial work debit")
	}
	if !leaf.tryClassWork(3) || root.classWorkUsed != classProofCandidateWorkLimit || child.classWorkUsed != 3 || leaf.classWorkUsed != 3 {
		t.Fatal("exact remaining ancestor work was not admitted atomically")
	}
	sibling := root.Child(128 << 20)
	if sibling.tryClassWork(1) || sibling.classWorkUsed != 0 {
		t.Fatal("sibling bypassed exhausted candidate")
	}
	root.classWorkUsed = 0
	child.classWorkUsed = classProofCandidateWorkLimit
	if leaf.tryClassWork(1) || root.classWorkUsed != 0 || leaf.classWorkUsed != 3 {
		t.Fatal("exhausted intermediate child changed another ancestor")
	}
	if root.tryClassWork(-1) || root.classWorkUsed != 0 {
		t.Fatal("negative work credit accepted")
	}
	p, err := compileProgram(`^[a-z]+$`)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []*ProgramBudget{child, NewProgramBudgetWithoutClassProof(128 << 20).Child(128 << 20)} {
		before := budget.classWorkUsed
		a, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, budget)
		if err != nil || !asciiIdentity(a) || budget.classWorkUsed != before {
			t.Fatal("inherited exhaustion refused a rule or retained partial proof", err)
		}
		if a.matchMask("abc") != 1 || a.matchMask("123") != 0 {
			t.Fatal("ancestor fallback changed language")
		}
	}
	if root.classWorkUsed != 0 {
		t.Fatal("child-refused proof charged its parent")
	}
}

func TestAutomatonASCIIProofRewind(t *testing.T) {
	root := NewProgramBudget(128 << 20)
	child := root.Child(128 << 20)
	if err := child.take(500); err != nil {
		t.Fatal(err)
	}
	if !child.tryClassWork(700) {
		t.Fatal("work admission failed")
	}
	child.Rewind(0)
	if root.Used() != 0 || child.Used() != 0 || root.classWorkUsed != 700 || child.classWorkUsed != 700 {
		t.Fatal("byte rewind refunded spent proof work")
	}
	validation := NewProgramBudgetWithoutClassProof(128 << 20)
	if err := validation.take(500); err != nil {
		t.Fatal(err)
	}
	validation.Rewind(0)
	if validation.tryClassWork(1) || validation.classWorkUsed != classProofCandidateWorkLimit || validation.Used() != 0 {
		t.Fatal("validation rewind re-enabled proof")
	}
	work := asciiProofWork{used: classProofMachineWorkLimit, budget: root}
	if work.take(1) || root.classWorkUsed != 700 {
		t.Fatal("local refusal debited ancestor work")
	}
	root.classWorkUsed = classProofCandidateWorkLimit
	work = asciiProofWork{budget: child}
	if work.take(1) || work.used != 0 || child.classWorkUsed != 700 {
		t.Fatal("ancestor refusal debited local work")
	}
}

func TestAutomatonASCIIUnicodeFallback(t *testing.T) {
	expressions := []string{`(?i)^k+$`, `(?i)s`, `\bword\b`, `(?m)^a$`, `(?s)^a.*z$`, `\p{Greek}+`, "�", `^[a-z]+$`}
	for _, cache := range []int{0, 512, 4096, 65536} {
		a, oracles := oracleMachine(t, expressions, cache)
		for range 3 {
			for _, name := range []string{"", "abc", "K", "k", "K", "kKk", "ſ", "s", "Σσς", "word", "éword", "a\nz", "a\xffz", "\xff", "�", "a\xc0\x80z", "\nword\n", "a\x00z"} {
				if got, want := a.matchMask(name), oracleMask(oracles, name); got != want {
					t.Fatalf("cache%d Unicode/fallback parity %q: %b != %b", cache, name, got, want)
				}
			}
		}
	}
	e, err := CompileRegex(`(?i)^k+$`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := e.MatchStringContext(ctx, "K"); got || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled diagnostic became language truth", err)
	}
}

func TestAutomatonASCIIClassBudget(t *testing.T) {
	p, err := compileProgram(`^[a-z]+[.]$`)
	if err != nil {
		t.Fatal(err)
	}
	full, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unsafe.Sizeof(full.asciiClasses) != 128 {
		t.Fatal("unexpected retained map size")
	}
	reserved := int64(full.bytes())
	for _, extra := range []int64{0, 255, 256} {
		budget := NewProgramBudget(reserved + extra)
		a, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, budget)
		if err != nil || budget.Used() != reserved || int64(a.bytes()) != reserved {
			t.Fatal("scratch retained or exact map admission failed", extra, err, budget.Used())
		}
		if extra < 256 && (!asciiIdentity(a) || budget.classWorkUsed != 0) {
			t.Fatal("unreserved optional scratch was used", extra)
		}
		if extra == 256 && (asciiIdentity(a) || budget.classWorkUsed == 0) {
			t.Fatal("reserved scratch did not prove small language")
		}
		if a.matchMask("abc.") != 1 || a.matchMask("abc") != 0 {
			t.Fatal("optional scratch changed language")
		}
	}
	root := NewProgramBudget(reserved + 255)
	child := root.Child(reserved + 256)
	a, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, child)
	if err != nil || !asciiIdentity(a) || root.Used() != reserved || child.Used() != reserved {
		t.Fatal("scratch shortage in ancestor refused or leaked byte reservation", err)
	}
	short := NewProgramBudget(reserved - 1)
	if got, err := newBudgetedAutomaton([]*syntax.Prog{p}, 4096, short); got != nil || !errors.Is(err, ErrProgramBudget) || short.Used() != 0 {
		t.Fatal("mandatory retained byte shortage was not typed and atomic", err)
	}
	for _, cache := range []int{0, 512, 4096, 65536} {
		budget := NewProgramBudget(128 << 20)
		m, err := newBudgetedAutomaton([]*syntax.Prog{p}, cache, budget)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(unsafe.Sizeof(*m)) + int64(len(p.Inst))*(int64(unsafe.Sizeof(syntax.Inst{}))+24) + 4
		if cache >= 512 {
			want += int64(cache)
		}
		if budget.Used() != want || int64(m.bytes()) != want {
			t.Fatal("map/header/arena was not accounted exactly once", cache, budget.Used(), want)
		}
		if cache == 0 && (!asciiIdentity(m) || budget.classWorkUsed != 0) {
			t.Fatal("disabled cache spent optional proof work")
		}
	}
}

// The full alphabet occurs after a prefix absent from the one-byte saturation
// queries. Every byte has a distinct global consuming predicate while those
// initial misses reuse the same empty subset/category until the table freezes.
func asciiDistinctSaturationExpression() string {
	var expression strings.Builder
	expression.WriteString("^~(?:")
	for symbol := range 128 {
		if symbol != 0 {
			expression.WriteByte('|')
		}
		fmt.Fprintf(&expression, `\x%02x%02x`, symbol, symbol)
	}
	expression.WriteString(")$")
	return expression.String()
}
