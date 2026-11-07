package blocking

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

const (
	maxDiagnosticExclusions = 1024
	maxDiagnosticText       = 1 << 20
	maxDiagnosticRule       = 4096
	maxDiagnosticPatterns   = 64
	maxDiagnosticWork       = 4096
	// MaxExclusionMatches bounds one diagnostic response independently of list size.
	MaxExclusionMatches = 64
)

// ExclusionMatch is evidence that an actual source rule was switched off and
// matches this question. It does not assert that the list or final answer is
// allowed: another rule can still decide. Source is the compiled source index,
// never its URL. Effect is block, allow, exception or rewrite.
type ExclusionMatch struct {
	List   string
	Source int
	Rule   string
	Effect string
}

type excludedRule struct {
	source  int
	text    string
	pattern string
	kind    parser.Kind
	except  bool
	regex   *regexp.Regexp
	slow    *compiledRule
}

type exclusionSet struct {
	rules     []excludedRule
	seen      map[string]bool
	bytes     int
	patterns  int
	truncated bool
}

func (s *exclusionSet) reserve(source int, text string, pattern bool) bool {
	if len(text) > maxDiagnosticRule || len(s.rules) >= maxDiagnosticExclusions ||
		len(text) > maxDiagnosticText-s.bytes || (pattern && s.patterns >= maxDiagnosticPatterns) {
		s.truncated = true
		return false
	}
	key := strconv.Itoa(source) + "/" + text
	if s.seen[key] {
		return false
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[key] = true
	s.bytes += len(text)
	if pattern {
		s.patterns++
	}
	return true
}

func (s *exclusionSet) addBasic(source int, rule parser.Rule) {
	// IP entries match response addresses rather than this question. This
	// endpoint reports question exclusions; it does not infer response matches.
	if rule.Kind == parser.KindIP {
		return
	}
	text := rule.String()
	if !s.reserve(source, text, rule.Kind == parser.KindRegex) {
		return
	}
	r := excludedRule{source: source, text: text, pattern: strings.Clone(rule.Pattern), kind: rule.Kind, except: rule.Exception}
	if rule.Kind == parser.KindRegex {
		var err error
		r.regex, err = matcher.CompileRegex(rule.Pattern)
		if err != nil {
			s.truncated = true
			return
		}
	}
	s.rules = append(s.rules, r)
}

func (s *exclusionSet) addSlow(source int, rule parser.UnsupportedRule) {
	if !s.reserve(source, rule.Text, true) {
		return
	}
	r, err := compileRule(rule)
	if err != nil {
		s.truncated = true
		return
	}
	if r.badfilter {
		return // a cancellation directive is not a question-matching rule
	}
	s.rules = append(s.rules, excludedRule{source: source, text: r.text, except: r.exception, slow: r})
}

func (r *excludedRule) matches(ctx Context) bool {
	if r.slow != nil {
		return r.slow.matches(ctx)
	}
	switch r.kind {
	case parser.KindExact:
		return ctx.Name == r.pattern
	case parser.KindSuffix:
		return ctx.Name == r.pattern || strings.HasSuffix(ctx.Name, "."+r.pattern)
	case parser.KindRegex:
		return r.regex.MatchString(ctx.Name)
	}
	return false
}

// ExcludedQuestionRules examines only this immutable policy selection, with a
// bounded scan and response. Call only for explicit diagnostics, never ordinary
// DNS traffic. Truncated means the evidence may be incomplete; no match is not
// proof of absence when it is true. This cannot change the filtering decision.
func (sel *Selection) ExcludedQuestionRules(ctx context.Context, question Context) ([]ExclusionMatch, bool) {
	if sel == nil {
		return nil, false
	}
	question.Name = NormalizeName(question.Name)
	var out []ExclusionMatch
	truncated, work := false, 0
	for _, lists := range [][]*List{sel.allow, sel.block} {
		for _, list := range lists {
			work++
			if work > maxDiagnosticWork || ctx.Err() != nil {
				return out, true
			}
			truncated = truncated || list.exclusions.truncated
			for _, rule := range list.exclusions.rules {
				work++
				if work > maxDiagnosticWork || ctx.Err() != nil {
					return out, true
				}
				if !rule.matches(question) {
					continue
				}
				if len(out) == MaxExclusionMatches {
					return out, true
				}
				effect := "block"
				switch {
				case list.mode == ModeAllow:
					effect = "allow"
				case rule.except:
					effect = "exception"
				case rule.slow != nil && rule.slow.rewrite != nil:
					effect = "rewrite"
				}
				out = append(out, ExclusionMatch{List: list.name, Source: rule.source, Rule: rule.text, Effect: effect})
			}
		}
	}
	return out, truncated
}
