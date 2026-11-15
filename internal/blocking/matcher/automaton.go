package matcher

import (
	"context"
	"errors"
	"math/bits"
	"regexp/syntax"
	"sync"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// ErrProgramBudget distinguishes resource refusal from invalid rule syntax.
// Callers must reject the candidate instead of counting this as an invalid rule.
var ErrProgramBudget = errors.New("matcher: compiled expression budget exceeded")

// ErrExecutionBusy refuses optional diagnostic work without waiting behind a
// shared exact fallback. It never represents a language miss or DNS decision.
var ErrExecutionBusy = errors.New("matcher: diagnostic expression executor busy")

const (
	automatonRules                   = 64 // one result word; larger collections use ordered shards
	programInstLimit                 = 32768
	programRuneLimit                 = 262144
	expressionSourceLimit            = 64 << 10
	propertyEscapeLimit              = 256
	defaultTransitionBytes           = 4096
	classProofMachineWorkLimit int64 = 1 << 20
	classProofScratchBytes     int64 = 256
)

// compileProgram uses public Go syntax APIs, not Go's regexp execution engine.
// The pre-simplification walk bounds repeat expansion before allocating it.
func compileProgram(src string) (*syntax.Prog, error) {
	if !parseInputFits(src) {
		return nil, ErrProgramBudget
	}
	re, err := syntax.Parse(src, syntax.Perl)
	if err != nil {
		var syntaxErr *syntax.Error
		if errors.As(err, &syntaxErr) && (syntaxErr.Code == syntax.ErrLarge || syntaxErr.Code == syntax.ErrNestingDepth) {
			return nil, ErrProgramBudget
		}
		return nil, err
	}
	inst, runes := programCost(re)
	if inst > programInstLimit || runes > programRuneLimit {
		return nil, ErrProgramBudget
	}
	p, err := syntax.Compile(re.Simplify())
	if err != nil {
		return nil, err
	}
	if len(p.Inst) > programInstLimit {
		return nil, ErrProgramBudget
	}
	return p, nil
}

// Unicode properties can expand to large rune tables while parsing, before an
// AST size check is possible. Bound their introducers before entering the
// public parser. Counting a quoted introducer conservatively is intentional:
// exceeding this resource limit refuses the whole candidate, never a rule.
func parseInputFits(src string) bool {
	if len(src) > expressionSourceLimit {
		return false
	}
	properties := 0
	for i := 0; i+1 < len(src); i++ {
		if src[i] == '\\' {
			if src[i+1] == 'p' || src[i+1] == 'P' {
				properties++
				if properties > propertyEscapeLimit {
					return false
				}
			}
			i++
		}
	}
	return true
}

// Saturating arithmetic avoids overflow even for nested counted repetitions.
func boundedAdd(a, b, limit int) int {
	if a >= limit+1 || b >= limit+1-a {
		return limit + 1
	}
	return a + b
}

func programCost(re *syntax.Regexp) (inst, runes int) {
	inst, runes = 2+len(re.Rune), len(re.Rune)
	for _, sub := range re.Sub {
		i, r := programCost(sub)
		inst = boundedAdd(inst, i, programInstLimit)
		runes = boundedAdd(runes, r, programRuneLimit)
	}
	if re.Op == syntax.OpRepeat {
		n := re.Max
		if n < 0 {
			n = re.Min + 1
		}
		for i, r := inst, runes; n > 1; n-- {
			inst = boundedAdd(inst, i, programInstLimit)
			runes = boundedAdd(runes, r, programRuneLimit)
			if inst > programInstLimit || runes > programRuneLimit {
				break
			}
		}
	}
	return inst, runes
}

type dfaState struct {
	start, count int
	previous     rune
	hash         uint64
}

type dfaTransition struct {
	from, to uint32 // zero means vacant; IDs are append-only
	symbol   uint16
	accept   uint64
}

// automaton is an exact tagged Thompson graph with a bounded lazy subset DFA.
// Programs/tags are immutable. All derived storage, including NFA fallback
// scratch, is reserved at construction; matching does not allocate. A mutex
// bounds scratch independently of concurrent callers. Callbacks run afterward.
type automaton struct {
	reservedBytes               int // immutable after construction; safe during matching
	mu                          sync.Mutex
	inst                        []syntax.Inst
	starts                      []uint32
	states                      []dfaState
	stateSlots                  []uint32
	pcs                         []uint32
	transitions                 []dfaTransition
	stateCount, pcCount         int
	frozen                      bool
	beginTextOnly               bool // immutable public StartCond proof for every program
	builds                      uint64
	current, next, stack, ready []uint32
	seen, nextSeen              []uint32
	epoch                       uint32
	asciiClasses                [128]uint8
}

func newAutomaton(programs []*syntax.Prog, cacheBytes int) (*automaton, error) {
	return newBudgetedAutomaton(programs, cacheBytes, nil)
}

func newBudgetedAutomaton(programs []*syntax.Prog, cacheBytes int, budget *ProgramBudget) (*automaton, error) {
	if len(programs) > automatonRules || cacheBytes < 0 || cacheBytes > 1<<20 {
		return nil, ErrProgramBudget
	}
	a := &automaton{beginTextOnly: len(programs) != 0}
	count := 0
	for _, p := range programs {
		if p == nil || len(p.Inst) > programInstLimit || len(p.Inst) > automatonRules*programInstLimit-count {
			return nil, ErrProgramBudget
		}
		count += len(p.Inst)
		if p.StartCond()&syntax.EmptyBeginText == 0 {
			a.beginTextOnly = false
		}
	}
	// Reserve before allocating persistent graph/scratch/cache arrays. Rune
	// slices are borrowed from the expression programs and charged there.
	slots := 0
	if cacheBytes >= 512 {
		slots = 1 << (bits.Len(uint(cacheBytes/128)) - 1)
	}
	reserved := int(unsafe.Sizeof(*a)) + count*(int(unsafe.Sizeof(syntax.Inst{}))+24) + 4*len(programs)
	if slots != 0 {
		reserved += cacheBytes
	}
	if err := budget.take(int64(reserved)); err != nil {
		return nil, err
	}
	a.reservedBytes = reserved
	a.identityASCIIClasses()
	a.starts = make([]uint32, 0, len(programs))
	a.inst = make([]syntax.Inst, 0, count)
	for id, p := range programs {
		base := len(a.inst)
		a.starts = append(a.starts, uint32(base+p.Start)) // #nosec G115 -- compiled start PC plus base is below 64 * 32768 instructions
		for _, original := range p.Inst {
			in := original
			switch in.Op {
			case syntax.InstAlt, syntax.InstAltMatch:
				in.Arg += uint32(base) // #nosec G115 -- base is below the 64 * 32768 instruction limit
				in.Out += uint32(base) // #nosec G115 -- base is below the 64 * 32768 instruction limit
			case syntax.InstMatch:
				in.Arg = uint32(id)
			case syntax.InstFail:
			default:
				in.Out += uint32(base) // #nosec G115 -- base is below the 64 * 32768 instruction limit
			}
			a.inst = append(a.inst, in)
		}
	}
	a.current = make([]uint32, 0, count)
	a.next = make([]uint32, 0, count)
	a.stack = make([]uint32, 0, count)
	a.ready = make([]uint32, 0, count)
	a.seen = make([]uint32, count)
	a.nextSeen = make([]uint32, count)
	// The table/arena split is fixed, including headers. No dynamic map grows
	// behind this budget. Very small budgets simply execute the exact NFA.
	if slots != 0 {
		a.states = make([]dfaState, slots)
		a.stateSlots = make([]uint32, slots*2)
		a.transitions = make([]dfaTransition, slots*2)
		used := int(unsafe.Sizeof(dfaState{}))*slots + 4*slots*2 + int(unsafe.Sizeof(dfaTransition{}))*slots*2
		a.pcs = make([]uint32, max(0, cacheBytes-used)/4)
		a.states[0] = dfaState{previous: -1}
		a.stateCount = 1 // start has no previously active PCs
	} else {
		a.frozen = true
	}
	a.prepareASCIIClasses(budget)
	return a, nil
}

func (a *automaton) identityASCIIClasses() {
	for symbol := range a.asciiClasses {
		a.asciiClasses[symbol] = uint8(symbol)
	}
}

// asciiProofWork is compile-only and never retained by a published machine.
// Its local and ancestor debits both succeed before any charged work begins.
type asciiProofWork struct {
	used   int64
	budget *ProgramBudget
}

func (w *asciiProofWork) take(n int64) bool {
	if n < 0 || w.used < 0 || w.used > classProofMachineWorkLimit || n > classProofMachineWorkLimit-w.used {
		return false
	}
	if !w.budget.tryClassWork(n) {
		return false
	}
	w.used += n
	return true
}

func (a *automaton) prepareASCIIClasses(budget *ProgramBudget) {
	if len(a.transitions) == 0 {
		return
	}
	// Optional scratch shares the byte ceiling; lack of headroom leaves the
	// already charged identity map instead of refusing a language or rule.
	mark := budget.Used()
	if err := budget.take(classProofScratchBytes); err != nil {
		return
	}
	work := asciiProofWork{budget: budget}
	if !a.refineASCIIClasses(&work) {
		a.identityASCIIClasses()
	}
	budget.Rewind(mark) // bytes only; spent proof work remains charged
}

// asciiPredicateCost precharges a conservative comparison/folding bound before
// public MatchRune calls. A large Rune table can decline by length alone; no
// unmetered table scan is hidden behind a predicate evaluation.
func asciiPredicateCost(in *syntax.Inst, work *asciiProofWork) (int64, bool) {
	if in.Op == syntax.InstRuneAny || in.Op == syntax.InstRuneAnyNotNL {
		return 1, true
	}
	length := int64(len(in.Rune))
	if length > classProofMachineWorkLimit/2-1 {
		return 0, false
	}
	cost := 2*length + 2
	if length == 1 && syntax.Flags(in.Arg)&syntax.FoldCase != 0 { // #nosec G115 -- syntax compiler stores only the FoldCase flag for rune instructions
		// Discover the actual public Unicode cycle, including non-ASCII
		// members, with a bound before each step. Never assume its size.
		start, current := in.Rune[0], in.Rune[0]
		cycle := int64(0)
		for {
			if !work.take(2) {
				return 0, false
			}
			current = unicode.SimpleFold(current)
			cycle++
			if current == start {
				break
			}
		}
		cost = 2*cycle + 2
	}
	return cost, true
}

// refineASCIIClasses only splits by exact current-boundary and consuming
// predicates. EOF is outside this map. Failure requires restoring full identity;
// no partial proof is published. The caller reserves the fixed scratch first.
func (a *automaton) refineASCIIClasses(work *asciiProofWork) bool {
	if !work.take(128 + 256) {
		return false
	}
	var refined [256]uint8
	for symbol := range a.asciiClasses {
		switch boundaryRune(rune(symbol)) {
		case '\n':
			a.asciiClasses[symbol] = 0
		case 'a':
			a.asciiClasses[symbol] = 1
		default:
			a.asciiClasses[symbol] = 2
		}
	}
	for i := range a.inst {
		if !work.take(1) {
			return false
		}
		in := &a.inst[i]
		switch in.Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		default:
			continue
		}
		cost, ok := asciiPredicateCost(in, work)
		if !ok || cost > (classProofMachineWorkLimit-256)/128-4 || !work.take(256+128*(cost+4)) {
			return false
		}
		for key := range refined {
			refined[key] = 255
		}
		classes := 0
		for symbol := range a.asciiClasses {
			matched := false
			switch in.Op {
			case syntax.InstRuneAny:
				matched = true
			case syntax.InstRuneAnyNotNL:
				matched = symbol != '\n'
			default:
				matched = in.MatchRune(rune(symbol))
			}
			key := int(a.asciiClasses[symbol]) * 2
			if matched {
				key++
			}
			if refined[key] == 255 {
				refined[key] = uint8(classes)
				classes++
			}
			a.asciiClasses[symbol] = refined[key]
		}
		if classes == 128 {
			return true // a discrete partition cannot split further
		}
	}
	return true
}

func boundaryRune(r rune) rune {
	switch {
	case r < 0:
		return -1
	case r == '\n':
		return '\n'
	case syntax.IsWordChar(r):
		return 'a'
	default:
		return '-'
	}
}

func (a *automaton) tick() {
	a.epoch++
	if a.epoch == 0 {
		clear(a.seen)
		clear(a.nextSeen)
		a.epoch = 1
	}
}

// step visits every reachable instruction at most once at this boundary.
func (a *automaton) step(previous, r rune) uint64 {
	a.tick()
	a.stack, a.ready, a.next = a.stack[:0], a.ready[:0], a.next[:0]
	push := func(pc uint32) {
		if a.seen[pc] != a.epoch {
			a.seen[pc] = a.epoch
			a.stack = append(a.stack, pc)
		}
	}
	for _, pc := range a.current {
		push(pc)
	}
	// Every proven anchored program can start only at the first boundary.
	// Current PCs still advance normally, including any internal assertions.
	if !a.beginTextOnly || previous < 0 {
		for _, pc := range a.starts {
			push(pc)
		}
	}
	context := syntax.EmptyOpContext(previous, r)
	var accept uint64
	for len(a.stack) != 0 {
		pc := a.stack[len(a.stack)-1]
		a.stack = a.stack[:len(a.stack)-1]
		in := &a.inst[pc]
		switch in.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			push(in.Out)
			push(in.Arg)
		case syntax.InstCapture, syntax.InstNop:
			push(in.Out)
		case syntax.InstEmptyWidth:
			if syntax.EmptyOp(in.Arg)&context == syntax.EmptyOp(in.Arg) { // #nosec G115 -- syntax compiler stores only defined EmptyOp bits in this instruction
				push(in.Out)
			}
		case syntax.InstMatch:
			accept |= uint64(1) << in.Arg
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			a.ready = append(a.ready, pc)
		}
	}
	if r >= 0 {
		for _, pc := range a.ready {
			in := &a.inst[pc]
			match := in.MatchRune(r)
			if in.Op == syntax.InstRuneAny {
				match = true
			}
			if in.Op == syntax.InstRuneAnyNotNL {
				match = r != '\n'
			}
			if match && a.nextSeen[in.Out] != a.epoch {
				a.nextSeen[in.Out] = a.epoch
				a.next = append(a.next, in.Out)
			}
		}
	}
	a.current, a.next = a.next, a.current
	return accept
}

func subsetHash(pcs []uint32, previous rune) uint64 {
	h := uint64(previous+2) + 0x9e3779b97f4a7c15 // #nosec G115 -- boundaryRune produces only -1, newline, a, or hyphen
	for _, pc := range pcs {
		// Set identity is independent of closure traversal order. Exact
		// membership is checked below; hash collisions can only cost work.
		h ^= bits.RotateLeft64(uint64(pc+1)*0x9e3779b97f4a7c15, int(pc%63))
	}
	return h
}

func (a *automaton) intern(previous rune) uint32 {
	previous = boundaryRune(previous)
	h := subsetHash(a.current, previous)
	for probe := range min(16, len(a.stateSlots)) {
		slot := int((h + uint64(probe)) & uint64(len(a.stateSlots)-1)) // #nosec G115 -- loop requires a nonempty bounded slot table; mask bounds the index
		id := a.stateSlots[slot]
		if id != 0 {
			s := &a.states[id-1]
			if s.hash == h && s.previous == previous && s.count == len(a.current) {
				same := true
				// step marked exactly the successor set with this epoch.
				// Membership checking is linear, with no sorting/allocation.
				for _, pc := range a.pcs[s.start : s.start+s.count] {
					if a.nextSeen[pc] != a.epoch {
						same = false
						break
					}
				}
				if same {
					return id
				}
			}
			continue
		}
		if a.stateCount == len(a.states) || len(a.current) > len(a.pcs)-a.pcCount {
			break
		}
		a.states[a.stateCount] = dfaState{start: a.pcCount, count: len(a.current), previous: previous, hash: h}
		copy(a.pcs[a.pcCount:], a.current)
		a.pcCount += len(a.current)
		a.stateCount++
		id = uint32(a.stateCount) // #nosec G115 -- count is bounded by at most 8192 reserved states
		a.stateSlots[slot] = id
		return id
	}
	a.frozen = true
	return 0
}

func (a *automaton) transition(from uint32, symbol uint16) *dfaTransition {
	if len(a.transitions) == 0 {
		return nil
	}
	h := uint64(from)*0x9e3779b1 + uint64(symbol)*0x85ebca6b
	for probe := range min(16, len(a.transitions)) {
		t := &a.transitions[int((h+uint64(probe))&uint64(len(a.transitions)-1))] // #nosec G115 -- nonempty bounded table; mask bounds the index
		if t.from == 0 || t.from == from && t.symbol == symbol {
			return t
		}
	}
	return nil
}

func (a *automaton) matchMask(name string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	mask, _ := a.matchLocked(nil, name) //nolint:staticcheck // SA1012: nil deliberately selects the uncancelable fast path; diagnostic calls retain their original context
	return mask
}

func (a *automaton) matchMaskContext(ctx context.Context, name string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !a.mu.TryLock() {
		return 0, ErrExecutionBusy
	}
	defer a.mu.Unlock()
	return a.matchLocked(ctx, name)
}

func (a *automaton) matchLocked(ctx context.Context, name string) (uint64, error) {
	a.current = a.current[:0]
	previous, state := rune(-1), uint32(0)
	if a.stateCount != 0 {
		state = 1
	}
	var result uint64
	for pos := 0; ; {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		r, size := rune(-1), 0
		if pos < len(name) {
			r, size = utf8.DecodeRuneInString(name[pos:])
		}
		symbol := uint16(128) // EOF cannot collide with ASCII NUL
		if r >= 0 && r < 128 {
			symbol = uint16(a.asciiClasses[r])
		}
		var transition *dfaTransition
		if state != 0 && r < 128 {
			transition = a.transition(state, symbol)
			if transition == nil {
				a.frozen = true
			}
			if transition != nil && transition.from != 0 {
				result |= transition.accept
				state = transition.to
				// An empty successor cannot restart after consuming a rune
				// when every program requires the beginning of the text.
				if r < 0 || a.beginTextOnly && state != 0 && a.states[state-1].count == 0 {
					break
				}
				previous = boundaryRune(r)
				pos += size
				continue
			}
		}
		if state != 0 {
			s := a.states[state-1]
			a.current = append(a.current[:0], a.pcs[s.start:s.start+s.count]...)
		}
		accept := a.step(previous, r)
		result |= accept
		next := uint32(0)
		if !a.frozen && transition != nil && r < 128 {
			if r >= 0 {
				next = a.intern(r)
			}
			if !a.frozen {
				*transition = dfaTransition{from: state, to: next, symbol: symbol, accept: accept}
				a.builds++
			}
		}
		state = next
		// Keep tags accepted at this boundary, then reach the shared final
		// cancellation check even when no anchored path can continue.
		if r < 0 || a.beginTextOnly && len(a.current) == 0 {
			break
		}
		previous = r
		pos += size
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	return result, nil
}

func (a *automaton) bytes() int {
	return a.reservedBytes
}
