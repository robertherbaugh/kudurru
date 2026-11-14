package blocking

import (
	"context"
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
	regex   *matcher.Expression
	slow    *compiledRule
}

type exclusionSet struct {
	rules     []excludedRule
	seen      map[string]bool
	bytes     int
	patterns  int
	truncated bool
	budget    *matcher.ProgramBudget
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
		mark := s.budget.Used()
		r.regex, err = matcher.CompileRegexWithBudget(rule.Pattern, s.budget)
		if err != nil {
			s.budget.Rewind(mark)
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
	mark := s.budget.Used()
	r, err := compileRuleBudget(rule, s.budget)
	if err != nil {
		s.budget.Rewind(mark)
		s.truncated = true
		return
	}
	if r.badfilter {
		s.budget.Rewind(mark)
		return // a cancellation directive is not a question-matching rule
	}
	s.rules = append(s.rules, excludedRule{source: source, text: r.text, except: r.exception, slow: r})
}

func (r *excludedRule) matches(ctx context.Context, question Context) (bool, error) {
	if r.slow != nil {
		if !r.slow.matchesContext(question) {
			return false, nil
		}
		return r.slow.pattern.MatchContext(ctx, question.Name)
	}
	switch r.kind {
	case parser.KindExact:
		return question.Name == r.pattern, nil
	case parser.KindSuffix:
		return question.Name == r.pattern || strings.HasSuffix(question.Name, "."+r.pattern), nil
	case parser.KindRegex:
		return r.regex.MatchStringContext(ctx, question.Name)
	}
	return false, nil
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
				matched, err := rule.matches(ctx, question)
				if err != nil {
					return out, true
				}
				if !matched {
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
