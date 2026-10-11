package matcher

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
)

// RegexWarnThreshold is the number of regexes per list above which callers
// should warn: every regex is evaluated for every query that the exact and
// suffix matchers did not answer (blocky uses the same threshold).
const RegexWarnThreshold = 500

// ErrRegexCap is returned by RegexBuilder.Add once the hard cap is reached.
var ErrRegexCap = errors.New("matcher: per-list regex hard cap reached")

// Regex is a linear scan over RE2 expressions. Each expression carries the
// longest literal that every match must contain; the scan checks that
// literal with strings.Contains before running the regex engine, which skips
// most expressions for most names.
//
// Expressions are matched against the normalised (lower-case) name. An
// expression that contains upper-case ASCII letters is compiled
// case-insensitively so that it can still match.
type Regex struct {
	entries []regexEntry
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

// Match returns the first expression, in insertion order, that matches name.
func (r *Regex) Match(name string) (src string, tag uint8, ok bool) {
	if r == nil {
		return "", 0, false
	}
	for i := range r.entries {
		e := &r.entries[i]
		if e.lit != "" && !strings.Contains(name, e.lit) {
			continue
		}
		if e.re.MatchString(name) {
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
	n := 0
	for i := range r.entries {
		n += 2*len(r.entries[i].src) + len(r.entries[i].lit) + 2048
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

// Add compiles src and adds it. Duplicate sources are ignored. It returns
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
	r := &Regex{entries: b.entries}
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
