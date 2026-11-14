package matcher

import (
	"context"
	"regexp/syntax"
	"strings"
	"unsafe"
)

// Expression is an exact RE2 language compiled by the shared tagged engine.
// It deliberately exposes no policy/client/source verdict or submatch state.
type Expression struct {
	source  string
	program *syntax.Prog
	machine *automaton
	bytes   int
}

func compileExpression(src string, budget *ProgramBudget) (*Expression, error) {
	p, err := compileProgram(src)
	if err != nil {
		return nil, err
	}
	n := int(unsafe.Sizeof(Expression{})) + len(src) + int(unsafe.Sizeof(syntax.Prog{})) + cap(p.Inst)*int(unsafe.Sizeof(syntax.Inst{}))
	for _, in := range p.Inst {
		n += len(in.Rune) * 4
	}
	if err = budget.take(int64(n)); err != nil {
		return nil, err
	}
	// syntax programs may contain overlapping subslices into parser-owned
	// literal buffers. Own precisely sized rune arrays so retained accounting
	// neither counts every suffix's capacity again nor retains hidden tails.
	for i := range p.Inst {
		if len(p.Inst[i].Rune) != 0 {
			owned := make([]rune, len(p.Inst[i].Rune))
			copy(owned, p.Inst[i].Rune)
			p.Inst[i].Rune = owned
		}
	}
	a, err := newBudgetedAutomaton([]*syntax.Prog{p}, defaultTransitionBytes, budget)
	if err != nil {
		return nil, err
	}
	return &Expression{source: strings.Clone(src), program: p, machine: a, bytes: n + a.bytes()}, nil
}

// MatchString reports an exact language match. Cache saturation changes work,
// never truth; callers retain all original policy/permission checks.
func (e *Expression) MatchString(name string) bool { return e.machine.matchMask(name) != 0 }

// MatchStringContext is for optional bounded diagnostics. Busy admission and
// cancellation are explicit errors; callers must not turn them into misses.
func (e *Expression) MatchStringContext(ctx context.Context, name string) (bool, error) {
	mask, err := e.machine.matchMaskContext(ctx, name)
	return mask != 0, err
}

// String returns the effective expression, including the original case wrapper.
func (e *Expression) String() string { return e.source }

func batchExpressions(expressions []*Expression, budget *ProgramBudget) ([]*automaton, error) {
	var out []*automaton
	for start := 0; start < len(expressions); start += automatonRules {
		end := min(start+automatonRules, len(expressions))
		programs := make([]*syntax.Prog, end-start)
		for i, e := range expressions[start:end] {
			programs[i] = e.program
		}
		a, err := newBudgetedAutomaton(programs, 64<<10, budget)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func prefilterSafe(name string) bool {
	for i := range len(name) {
		if name[i] >= 128 || name[i] >= 'A' && name[i] <= 'Z' {
			return false
		}
	}
	return true
}
