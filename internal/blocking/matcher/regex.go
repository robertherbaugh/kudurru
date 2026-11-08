package matcher

import (
	"errors"
	"fmt"
	"math/bits"
	"regexp/syntax"
	"strings"
	"unsafe"
)

// RegexWarnThreshold is the number of regexes per list above which callers
// should warn (blocky uses the same threshold).
const RegexWarnThreshold = 500

// RegexUnindexedWarnThreshold warns about rules without a required literal.
// They need shared execution on every regex lookup; a saturated lazy cache
// remains exact but executes the bounded NFA rather than cached transitions.
const RegexUnindexedWarnThreshold = 50

// ErrRegexCap is returned by RegexBuilder.Add once the hard cap is reached.
var ErrRegexCap = errors.New("matcher: per-list regex hard cap reached")

const (
	// gramLen is the length of the literal substrings ("grams") the regex
	// index is keyed by.
	gramLen = 4
	// gramBits sizes the bitmap in front of the gram index.
	gramBits = 1 << 16
	// maxCandidates bounds the candidate buffer of one lookup; dense
	// candidates use fixed ordered tagged batches.
	maxCandidates = 128
)

// Regex matches RE2 languages using the shared tagged lazy-DFA executor.
// Required-literal grams avoid execution on proven misses and select small
// sparse candidate sets. Dense/unindexed candidates use fixed ordered tagged
// sets, with exact bounded NFA execution after cache saturation.
//
// Expressions are matched against the normalised (lower-case) name. An
// expression that contains upper-case ASCII letters is compiled
// case-insensitively so that it can still match. When several expressions
// match, the first one added wins.
type Regex struct {
	entries []regexEntry
	always  []int32            // expressions without an indexed gram, ascending
	index   map[uint32][]int32 // gram -> expressions, ascending
	bits    []uint64           // gramBits-bit filter over the indexed grams
	batches []*automaton
	// The two batch lanes preserve original order within unindexed and
	// indexed rules. Local engine tags always map through batchIDs.
	batchIDs         [][]int32
	unindexedBatches int
	batchBytes       int // charged immutable mapping/slice storage
}

type regexEntry struct {
	re  *Expression
	src string // as written, without the /.../ delimiters
	lit string // required literal; empty when none could be derived
	tag uint8
}

// Len returns the number of unique expressions.
func (r *Regex) Len() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}

// Unindexed returns the number of expressions without an indexable literal,
// which are run for every lookup.
func (r *Regex) Unindexed() int {
	if r == nil {
		return 0
	}
	return len(r.always)
}

func gramAt(s string, i int) uint32 {
	return uint32(s[i]) | uint32(s[i+1])<<8 | uint32(s[i+2])<<16 | uint32(s[i+3])<<24
}

func gramBit(g uint32) uint32 {
	return (g * 0x9E3779B1) >> 16 // top 16 bits of a multiplicative hash
}

// Match returns the first expression, in insertion order, that matches name.
func (r *Regex) Match(name string) (src string, tag uint8, ok bool) {
	if r == nil {
		return "", 0, false
	}
	if !prefilterSafe(name) {
		return r.scanSafe(name, false)
	}
	var buf [maxCandidates]int32
	cand := buf[:0]
	for _, idx := range r.always {
		e := &r.entries[idx]
		if e.lit != "" && !strings.Contains(name, e.lit) {
			continue
		}
		if len(cand) == 16 {
			return r.scanSafe(name, true)
		}
		cand = append(cand, idx)
	}
	// A fixed unindexed batch is usable only when every original member
	// survived the same literal exclusions as the sparse candidate route.
	// No subset program or new retained query state is constructed here.
	sharedUnindexed := r.unindexedBatches == 1 && len(r.always) > 1 && len(cand) == len(r.always)
	for i := 0; i+gramLen <= len(name); i++ {
		g := gramAt(name, i)
		b := gramBit(g)
		if r.bits[b>>6]&(1<<(b&63)) == 0 {
			continue
		}
		for _, idx := range r.index[g] {
			e := &r.entries[idx]
			if e.lit != "" && !strings.Contains(name, e.lit) {
				continue
			}
			if len(cand) == 16 {
				return r.scanSafe(name, true)
			}
			cand = append(cand, idx)
		}
	}
	// Insertion sort: candidate lists are short, and evaluating in index
	// order keeps the first-added-wins rule of the linear scan.
	for i := 1; i < len(cand); i++ {
		for j := i; j > 0 && cand[j] < cand[j-1]; j-- {
			cand[j], cand[j-1] = cand[j-1], cand[j]
		}
	}
	prev := int32(-1)
	unindexedAt := 0
	var sharedMask uint64
	for _, idx := range cand {
		if idx == prev {
			continue
		}
		prev = idx
		e := &r.entries[idx]
		if sharedUnindexed && unindexedAt < len(r.batchIDs[0]) && idx == r.batchIDs[0][unindexedAt] {
			// Delay shared execution until its first original ID is reached;
			// an earlier indexed hit can still return without running it.
			if unindexedAt == 0 {
				sharedMask = r.batches[0].matchMask(name)
			}
			matched := sharedMask&(uint64(1)<<unindexedAt) != 0
			unindexedAt++
			if matched {
				return e.src, e.tag, true
			}
			continue
		}
		if (e.lit == "" || strings.Contains(name, e.lit)) && e.re.MatchString(name) {
			return e.src, e.tag, true
		}
	}
	return "", 0, false
}

// scan merges batch lower bounds to find the earliest original matching ID.
func (r *Regex) scan(name string) (string, uint8, bool) {
	return r.scanSafe(name, prefilterSafe(name))
}

// scanSafe reuses only the safety result already established by Match or scan.
func (r *Regex) scanSafe(name string, safe bool) (string, uint8, bool) {
	next := [2]int{0, r.unindexedBatches}
	end := [2]int{r.unindexedBatches, len(r.batches)}
	if safe && !r.hasIndexedGram(name) {
		end[1] = next[1]
	}
	best := len(r.entries)
	for {
		lane := -1
		for i := range next {
			if next[i] < end[i] && (lane < 0 || r.batchIDs[next[i]][0] < r.batchIDs[next[lane]][0]) { // #nosec G602 -- lane is -1 or a prior index from range next; short circuit excludes -1
				lane = i
			}
		}
		if lane < 0 {
			break
		}
		batch := next[lane] // #nosec G602 -- lane is a range-next index after the negative sentinel check
		next[lane]++        // #nosec G602 -- same checked range-next index
		if int(r.batchIDs[batch][0]) >= best {
			break // no remaining batch can contain an earlier original ID
		}
		if safe && r.batchExcluded(batch, name) {
			continue
		}
		if mask := r.batches[batch].matchMask(name); mask != 0 {
			id := int(r.batchIDs[batch][bits.TrailingZeros64(mask)])
			if id < best {
				best = id
			}
		}
	}
	if best < len(r.entries) {
		e := &r.entries[best]
		return e.src, e.tag, true
	}
	return "", 0, false
}

// hasIndexedGram reports possible indexed eligibility, not a language match.
// On prefilter-safe names every indexed match requires its selected literal
// gram. Bitmap positives must reach the exact index; collisions prove nothing.
// Callers may exclude the indexed lane only when the input is prefilter-safe.
func (r *Regex) hasIndexedGram(name string) bool {
	if len(r.index) == 0 {
		return false
	}
	for i := 0; i+gramLen <= len(name); i++ {
		g := gramAt(name, i)
		b := gramBit(g)
		if r.bits[b>>6]&(1<<(b&63)) != 0 && len(r.index[g]) != 0 {
			return true
		}
	}
	return false
}

// batchExcluded proves that every original member is a miss. Callers must
// bypass this shortcut for inputs outside prefilterSafe. An empty literal
// never excludes an expression, even if all its neighbors are excluded.
func (r *Regex) batchExcluded(batch int, name string) bool {
	for _, id := range r.batchIDs[batch] {
		e := &r.entries[id]
		if e.lit == "" || strings.Contains(name, e.lit) {
			return false
		}
	}
	return true
}

// Bytes approximates retained metadata plus reserved programs/cache/scratch.
func (r *Regex) Bytes() int {
	if r == nil {
		return 0
	}
	n := 8*len(r.bits) + 4*len(r.always) + 48*len(r.index) + r.batchBytes
	for i := range r.entries {
		n += 2*len(r.entries[i].src) + len(r.entries[i].lit) + r.entries[i].re.bytes + 4
	}
	for _, batch := range r.batches {
		n += batch.bytes()
	}
	return n
}

// ProgramBytes counts retained programs/cache/scratch and original-ID mapping
// storage, without unrelated entry/index metadata.
func (r *Regex) ProgramBytes() int {
	if r == nil {
		return 0
	}
	n := r.batchBytes
	for _, e := range r.entries {
		n += e.re.bytes
	}
	for _, batch := range r.batches {
		n += batch.bytes()
	}
	return n
}

// RegexBuilder accumulates expressions for a Regex matcher. The zero value
// has no cap.
type RegexBuilder struct {
	// HardCap is the maximum number of expressions; Add returns ErrRegexCap
	// beyond it. Zero or negative means unlimited.
	HardCap int
	// Budget charges all programs and reserved runtime capacity to one list.
	Budget *ProgramBudget

	entries []regexEntry
	seen    map[string]struct{}
	dropped int
}

// Add compiles src and adds a copy of it. Duplicate sources are ignored. It returns
// ErrRegexCap when the hard cap is reached and a wrapped compile error for
// an invalid expression.
func (b *RegexBuilder) Add(src string, tag uint8) error {
	if _, dup := b.seen[src]; dup {
		return nil
	}
	if b.HardCap > 0 && len(b.entries) >= b.HardCap {
		b.dropped++
		return ErrRegexCap
	}
	re, err := CompileRegexWithBudget(src, b.Budget)
	if err != nil {
		return fmt.Errorf("matcher: invalid regex %q: %w", src, err)
	}
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	src = strings.Clone(src) // the caller's string may alias a read buffer
	b.seen[src] = struct{}{}
	b.entries = append(b.entries, regexEntry{re: re, src: src, lit: RequiredLiteral(re.String()), tag: tag})
	return nil
}

// CompileRegex uses the list engine's case behavior for a normalized DNS name.
// Diagnostic matchers use it too so an excluded rule retains identical meaning.
func CompileRegex(src string) (*Expression, error) {
	return CompileRegexWithBudget(src, NewProgramBudget(DefaultProgramBytes))
}

// CompileRegexWithBudget uses the same case wrapper and a shared reservation.
func CompileRegexWithBudget(src string, budget *ProgramBudget) (*Expression, error) {
	if hasUpperASCII(src) {
		src = "(?i)" + src
	}
	return compileExpression(src, budget)
}

// Len returns the number of expressions added so far.
func (b *RegexBuilder) Len() int { return len(b.entries) }

// Has reports whether src was already added, before a shared cap is checked.
func (b *RegexBuilder) Has(src string) bool { _, ok := b.seen[src]; return ok }

// Dropped returns the number of Add calls rejected by the hard cap.
func (b *RegexBuilder) Dropped() int { return b.dropped }

// Build returns the matcher and resets the builder. It returns nil when
// nothing was added.
func (b *RegexBuilder) Build() *Regex {
	r, err := b.BuildChecked()
	if err != nil {
		panic(err)
	} // only callers with an explicit budget can refuse
	return r
}

// BuildChecked propagates aggregate program refusal to the list compiler.
func (b *RegexBuilder) BuildChecked() (*Regex, error) {
	if len(b.entries) == 0 {
		*b = RegexBuilder{HardCap: b.HardCap, Budget: b.Budget}
		return nil, nil
	}
	r := &Regex{entries: b.entries, index: map[uint32][]int32{}, bits: make([]uint64, gramBits/64)}
	for i, e := range r.entries {
		idx := int32(i) //nolint:gosec // bounded by the hard cap / memory
		if len(e.lit) < gramLen {
			r.always = append(r.always, idx)
			continue
		}
		// Index under the gram of the literal with the fewest expressions so
		// far, so that common grams (".com") do not collect long lists.
		best, bestLen := uint32(0), -1
		for j := 0; j+gramLen <= len(e.lit); j++ {
			g := gramAt(e.lit, j)
			if n := len(r.index[g]); bestLen < 0 || n < bestLen {
				best, bestLen = g, n
			}
		}
		r.index[best] = append(r.index[best], idx)
		bit := gramBit(best)
		r.bits[bit>>6] |= 1 << (bit & 63)
	}
	if err := r.buildBatches(b.Budget); err != nil {
		return nil, err
	}
	*b = RegexBuilder{HardCap: b.HardCap, Budget: b.Budget}
	return r, nil
}

// buildBatches keeps independently excludable indexed rules separate from
// always candidates. Every mask retains an explicit original-ID mapping.
func (r *Regex) buildBatches(budget *ProgramBudget) error {
	u := len(r.always)
	r.unindexedBatches = (u + automatonRules - 1) / automatonRules
	count := r.unindexedBatches + (len(r.entries)-u+automatonRules-1)/automatonRules
	// Reserve headers, exactly sized retained arrays, and the mapping's own
	// accounting fields before allocating them. The graph builder separately
	// charges every machine, including a possible extra partial batch.
	reserved := int64(unsafe.Sizeof(r.batches) + unsafe.Sizeof(r.batchIDs) + unsafe.Sizeof(r.unindexedBatches) + unsafe.Sizeof(r.batchBytes))
	reserved += int64(len(r.entries)) * int64(unsafe.Sizeof(int32(0)))
	reserved += int64(count) * int64(unsafe.Sizeof((*automaton)(nil))+unsafe.Sizeof([]int32{}))
	if err := budget.take(reserved); err != nil {
		return err
	}
	r.batchBytes = int(reserved)
	r.batches = make([]*automaton, 0, count)
	r.batchIDs = make([][]int32, 0, count)
	ids := make([]int32, len(r.entries))
	copy(ids, r.always)
	at := u
	for i, e := range r.entries {
		if len(e.lit) >= gramLen {
			ids[at] = int32(i) //nolint:gosec // programs bound entry count to available memory
			at++
		}
	}
	for _, group := range [2][]int32{ids[:u], ids[u:]} {
		for start := 0; start < len(group); start += automatonRules {
			end := min(start+automatonRules, len(group))
			members := group[start:end:end]
			var programs [automatonRules]*syntax.Prog
			for i, id := range members {
				programs[i] = r.entries[id].re.program
			}
			machine, err := newBudgetedAutomaton(programs[:len(members)], 64<<10, budget)
			if err != nil {
				return err
			}
			r.batches = append(r.batches, machine)
			r.batchIDs = append(r.batchIDs, members)
		}
	}
	return nil
}

// RequiredLiteral returns a conservative ASCII shortcut for normalized ASCII
// names, or "" when none is proven (alternations, optional parts, Unicode or
// resource/syntax refusal). Case-insensitive literals are lowercased. Callers
// must bypass shortcuts for other inputs, as Regex and PatternSet do.
func RequiredLiteral(expr string) string {
	if !parseInputFits(expr) {
		return ""
	}
	for i := range len(expr) {
		if expr[i] >= 128 {
			return ""
		}
	}
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return ""
	}
	// Required text does not require expansion of counted repetitions.
	return requiredLiteral(re)
}

func requiredLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		// Escaped Unicode is not visible in the source-byte preflight. Keep
		// every shortcut ASCII; Unicode inputs bypass shortcuts altogether.
		for _, r := range re.Rune {
			if r >= 128 {
				return ""
			}
		}
		s := string(re.Rune)
		if re.Flags&syntax.FoldCase != 0 {
			s = strings.ToLower(s)
		}
		return s
	case syntax.OpCapture, syntax.OpPlus:
		return requiredLiteral(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return requiredLiteral(re.Sub[0])
		}
	case syntax.OpConcat:
		best := ""
		for _, sub := range re.Sub {
			if s := requiredLiteral(sub); len(s) > len(best) {
				best = s
			}
		}
		return best
	}
	return ""
}

func hasUpperASCII(s string) bool {
	for i := range len(s) {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}

// Visit yields all matching expressions in insertion order. The rare
// cancellation path needs alternatives instead of the ordinary first hit.
func (r *Regex) Visit(name string, visit func(string, uint8) bool) {
	if r == nil {
		return
	}
	safe := prefilterSafe(name)
	cursors := [2]regexCursor{{end: r.unindexedBatches}, {batch: r.unindexedBatches, end: len(r.batches)}}
	if safe && !r.hasIndexedGram(name) {
		cursors[1].end = cursors[1].batch
	}
	var heads [2]int32
	var ready [2]bool
	for i := range cursors {
		heads[i], ready[i] = cursors[i].next(r, name, safe)
	}
	for ready[0] || ready[1] {
		lane := 0
		if !ready[0] || ready[1] && heads[1] < heads[0] {
			lane = 1
		}
		e := &r.entries[heads[lane]]
		if !visit(e.src, e.tag) {
			return
		}
		heads[lane], ready[lane] = cursors[lane].next(r, name, safe)
	}
}

// regexCursor streams one ordered lane with constant query storage. Each
// engine lock is released before a callback or another cursor is advanced.
type regexCursor struct {
	batch, end int
	mask       uint64
	ids        []int32
}

func (c *regexCursor) next(r *Regex, name string, safe bool) (int32, bool) {
	for {
		if c.mask != 0 {
			bit := bits.TrailingZeros64(c.mask)
			c.mask &= c.mask - 1
			return c.ids[bit], true
		}
		if c.batch >= c.end {
			return 0, false
		}
		batch := c.batch
		c.batch++
		if safe && r.batchExcluded(batch, name) {
			continue
		}
		c.ids = r.batchIDs[batch]
		c.mask = r.batches[batch].matchMask(name)
	}
}
