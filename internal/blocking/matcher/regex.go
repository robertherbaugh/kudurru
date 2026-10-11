package matcher

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
)

// RegexWarnThreshold is the number of regexes per list above which callers
// should warn (blocky uses the same threshold).
const RegexWarnThreshold = 500

// RegexUnindexedWarnThreshold is the number of unindexed expressions (no
// required literal of four bytes) per list above which callers should warn:
// each one runs for every name the exact and suffix sets did not answer, at
// roughly 25-75 ns apiece on the spike machine, so 50 of them cost 2-3.5 µs
// per lookup (docs/spikes/blocking.md).
const RegexUnindexedWarnThreshold = 50

// ErrRegexCap is returned by RegexBuilder.Add once the hard cap is reached.
var ErrRegexCap = errors.New("matcher: per-list regex hard cap reached")

const (
	// gramLen is the length of the literal substrings ("grams") the regex
	// index is keyed by.
	gramLen = 4
	// gramBits sizes the bitmap in front of the gram index.
	gramBits = 1 << 16
	// maxCandidates bounds the candidate buffer of one lookup; a name with
	// more candidates falls back to the linear scan.
	maxCandidates = 128
)

// Regex matches names against RE2 expressions. Each expression carries the
// longest literal that every match must contain (RequiredLiteral). Most
// expressions are indexed by one 4-byte gram of that literal, chosen to
// keep index buckets small, so a lookup slides over the grams of the name,
// tests a bitmap, and runs only the expressions whose gram occurs in the
// name: the cost follows the name length, not the number of expressions.
// Expressions without a usable literal are always run. The idea is the
// "shortcut index" of docs/architecture.md §7.3 item 4, re-implemented.
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
}

type regexEntry struct {
	re  *regexp.Regexp
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
	if len(r.always) > maxCandidates {
		return r.scan(name)
	}
	var buf [maxCandidates]int32
	cand := append(buf[:0], r.always...)
	for i := 0; i+gramLen <= len(name); i++ {
		g := gramAt(name, i)
		b := gramBit(g)
		if r.bits[b>>6]&(1<<(b&63)) == 0 {
			continue
		}
		ids := r.index[g]
		if len(cand)+len(ids) > maxCandidates {
			return r.scan(name)
		}
		cand = append(cand, ids...)
	}
	// Insertion sort: candidate lists are short, and evaluating in index
	// order keeps the first-added-wins rule of the linear scan.
	for i := 1; i < len(cand); i++ {
		for j := i; j > 0 && cand[j] < cand[j-1]; j-- {
			cand[j], cand[j-1] = cand[j-1], cand[j]
		}
	}
	prev := int32(-1)
	for _, idx := range cand {
		if idx == prev {
			continue
		}
		prev = idx
		e := &r.entries[idx]
		if (e.lit == "" || strings.Contains(name, e.lit)) && e.re.MatchString(name) {
			return e.src, e.tag, true
		}
	}
	return "", 0, false
}

// scan is the linear fallback for names with too many candidates.
func (r *Regex) scan(name string) (string, uint8, bool) {
	for i := range r.entries {
		e := &r.entries[i]
		if (e.lit == "" || strings.Contains(name, e.lit)) && e.re.MatchString(name) {
			return e.src, e.tag, true
		}
	}
	return "", 0, false
}

// Bytes returns a rough heap footprint estimate (the regexp program size is
// not exported, so this counts sources plus a fixed per-expression cost).
func (r *Regex) Bytes() int {
	if r == nil {
		return 0
	}
	n := 8*len(r.bits) + 4*len(r.always) + 48*len(r.index)
	for i := range r.entries {
		n += 2*len(r.entries[i].src) + len(r.entries[i].lit) + 2048 + 4
	}
	return n
}

// RegexBuilder accumulates expressions for a Regex matcher. The zero value
// has no cap.
type RegexBuilder struct {
	// HardCap is the maximum number of expressions; Add returns ErrRegexCap
	// beyond it. Zero or negative means unlimited.
	HardCap int

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
	expr := src
	if hasUpperASCII(src) {
		expr = "(?i)" + src
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fmt.Errorf("matcher: invalid regex %q: %w", src, err)
	}
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	src = strings.Clone(src) // the caller's string may alias a read buffer
	b.seen[src] = struct{}{}
	b.entries = append(b.entries, regexEntry{re: re, src: src, lit: RequiredLiteral(expr), tag: tag})
	return nil
}

// Len returns the number of expressions added so far.
func (b *RegexBuilder) Len() int { return len(b.entries) }

// Dropped returns the number of Add calls rejected by the hard cap.
func (b *RegexBuilder) Dropped() int { return b.dropped }

// Build returns the matcher and resets the builder. It returns nil when
// nothing was added.
func (b *RegexBuilder) Build() *Regex {
	if len(b.entries) == 0 {
		*b = RegexBuilder{HardCap: b.HardCap}
		return nil
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
	*b = RegexBuilder{HardCap: b.HardCap}
	return r
}

// RequiredLiteral returns the longest literal string that every match of the
// RE2 expression expr must contain, lower-cased when the literal is
// case-insensitive, or "" when none can be derived (alternations, optional
// parts, parse errors). It never returns a string that some match lacks, so
// it is safe as a prefilter.
func RequiredLiteral(expr string) string {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return ""
	}
	return requiredLiteral(re.Simplify())
}

func requiredLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
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
