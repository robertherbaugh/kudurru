package matcher

// DefaultProgramBytes is a list's compiled-expression reservation ceiling.
// It includes reserved transition/fallback capacity, not merely warm bytes.
// Complete serving preparation separately bounds all lists to one shared
// 128 MiB reservation. Neither limit is a total RSS or held-owner bound.
const DefaultProgramBytes int64 = 128 << 20

// Class proof is an optional constructor optimization, independently bounded
// across every list sharing a candidate. Exhaustion keeps exact identity keys.
const classProofCandidateWorkLimit int64 = 1 << 24

// ProgramBudget accounts retained expression storage during one compilation.
// It is single-owner, like the builders. Separate budgets can be chained so
// every reservation charges both a list and its complete serving candidate.
type ProgramBudget struct {
	limit, used   int64
	parent        *ProgramBudget
	classWorkUsed int64
}

// NewProgramBudget returns a finite compile budget. Nonpositive limits refuse
// all nonempty reservations; zero is never an unlimited production escape.
func NewProgramBudget(limit int64) *ProgramBudget { return &ProgramBudget{limit: limit} }

// NewProgramBudgetWithoutClassProof preserves the byte contract while skipping
// optional cache proof for temporary validation programs that will be discarded.
// Its children inherit the exhausted proof allowance through their parent.
func NewProgramBudgetWithoutClassProof(limit int64) *ProgramBudget {
	return &ProgramBudget{limit: limit, classWorkUsed: classProofCandidateWorkLimit}
}

// Child creates a separately bounded list whose reservations also charge b.
func (b *ProgramBudget) Child(limit int64) *ProgramBudget {
	return &ProgramBudget{limit: limit, parent: b}
}

// Used reports reserved bytes, including unused runtime capacity.
func (b *ProgramBudget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used
}

func (b *ProgramBudget) fits(n int64) bool {
	return b == nil || n >= 0 && b.used <= b.limit && n <= b.limit-b.used && b.parent.fits(n)
}

func (b *ProgramBudget) take(n int64) error {
	if !b.fits(n) {
		return ErrProgramBudget
	}
	for current := b; current != nil; current = current.parent {
		current.used += n
	}
	return nil
}

// tryClassWork checks every ancestor before debiting any of them. Compilation
// budgets have one owner; no partial debit survives a refused optional proof.
// Work already performed is never released by byte reservation checkpoints.
func (b *ProgramBudget) tryClassWork(n int64) bool {
	if n < 0 || n > classProofCandidateWorkLimit {
		return false
	}
	for current := b; current != nil; current = current.parent {
		if current.classWorkUsed < 0 || current.classWorkUsed > classProofCandidateWorkLimit || n > classProofCandidateWorkLimit-current.classWorkUsed {
			return false
		}
	}
	for current := b; current != nil; current = current.parent {
		current.classWorkUsed += n
	}
	return true
}

// Reserve accounts an already compiled list reused by the checksum shortcut.
func (b *ProgramBudget) Reserve(n int64) error { return b.take(n) }

// Rewind releases a single-owner compile phase's discarded reservations. It
// does not reclaim a published machine or change any live query's capacity.
// It never refunds optional proof work already performed by that phase.
func (b *ProgramBudget) Rewind(mark int64) {
	if b == nil || mark < 0 || mark > b.used {
		return
	}
	delta := b.used - mark
	for current := b; current != nil; current = current.parent {
		current.used -= delta
	}
}
