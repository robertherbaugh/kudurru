package matcher

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is a DNS-name adblock pattern compiled from the public filtering
// grammar. Browser URL/path rules are rejected by the parser, not broadened.
type Pattern struct {
	re      *regexp.Regexp
	literal string
}

// CompilePattern compiles a DNS adblock pattern or delimited RE2 expression.
func CompilePattern(pattern string) (*Pattern, error) {
	var expression strings.Builder
	rawRegex := len(pattern) > 2 && pattern[0] == '/' && pattern[len(pattern)-1] == '/'
	if rawRegex {
		expression.WriteString(pattern[1 : len(pattern)-1])
	} else {
		prefixOnly := strings.HasPrefix(pattern, "|") && !strings.HasPrefix(pattern, "||") && !strings.HasSuffix(pattern, "|") && !strings.HasSuffix(pattern, "^")
		simpleWildcard := strings.HasPrefix(pattern, "*.") && !strings.ContainsAny(pattern[2:], "*^|")
		if strings.HasPrefix(pattern, "||") {
			expression.WriteString(`(?:^|\.)`)
			pattern = pattern[2:]
			if strings.HasPrefix(pattern, "*.") && !strings.ContainsAny(strings.TrimSuffix(pattern[2:], "^"), "*^|") {
				pattern = pattern[2:]
			}
		} else if strings.HasPrefix(pattern, "|") {
			expression.WriteByte('^')
			pattern = pattern[1:]
		} else if simpleWildcard {
			expression.WriteString(`(?:^|\.)`)
			pattern = pattern[2:]
		}
		if !prefixOnly && !strings.ContainsAny(pattern, "*^|") {
			pattern += "^"
		}
		for i := 0; i < len(pattern); i++ {
			switch pattern[i] {
			case '*':
				expression.WriteString(".*")
			case '^':
				expression.WriteString(`(?:$|[^a-z0-9_.%-])`)
			case '|':
				if i != len(pattern)-1 {
					return nil, fmt.Errorf("invalid pattern anchor")
				}
				expression.WriteByte('$')
			default:
				expression.WriteString(regexp.QuoteMeta(string(pattern[i])))
			}
		}
	}
	re, err := regexp.Compile("(?i)" + expression.String())
	if err != nil {
		return nil, err
	}
	// RequiredLiteral handles alternation/optional segments conservatively;
	// a shortcut is never used unless every match must contain it.
	lit := RequiredLiteral(re.String())
	return &Pattern{re: re, literal: strings.ToLower(lit)}, nil
}

// Match verifies the complete compiled pattern against name.
func (p *Pattern) Match(name string) bool { return p.re.MatchString(name) }

// Literal returns a substring required by every match, or an empty string.
func (p *Pattern) Literal() string { return p.literal }

// PatternIndex selects all possible pattern IDs, with no truncation. A
// name's repeated shortcut is visited once. Entries lacking a safe shortcut
// are always visited. Callers still verify complete patterns and conditions.
type PatternIndex struct {
	grams  map[uint64][]int
	always []int
}

// NewPatternIndex indexes all supplied patterns without a candidate cap.
func NewPatternIndex(patterns []*Pattern) *PatternIndex {
	p := &PatternIndex{grams: map[uint64][]int{}}
	for i, pattern := range patterns {
		lit := pattern.Literal()
		if len(lit) < 5 {
			p.always = append(p.always, i)
			continue
		}
		// Pick the least-populated required gram instead of grouping every
		// host123... rule under the same leading literal.
		gram := gram5(lit, 0)
		for j := 1; j+5 <= len(lit); j++ {
			g := gram5(lit, j)
			if len(p.grams[g]) < len(p.grams[gram]) {
				gram = g
			}
		}
		p.grams[gram] = append(p.grams[gram], i)
	}
	return p
}
func gram5(s string, i int) uint64 {
	return uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 | uint64(s[i+4])<<32
}

// Visit yields each possible matching pattern ID once. Names are normalized.
func (p *PatternIndex) Visit(name string, visit func(int)) {
	if p == nil {
		return
	}
	for _, id := range p.always {
		visit(id)
	}
	// DNS names are bounded; checking preceding grams avoids allocating a
	// visited map and cannot drop candidates on a dense index bucket.
	for i := 0; i+5 <= len(name); i++ {
		g := gram5(name, i)
		ids := p.grams[g]
		if len(ids) == 0 {
			continue
		}
		repeated := false
		for j := 0; j < i; j++ {
			if j+5 <= len(name) && gram5(name, j) == g {
				repeated = true
				break
			}
		}
		if repeated {
			continue
		}
		for _, id := range ids {
			visit(id)
		}
	}
}
