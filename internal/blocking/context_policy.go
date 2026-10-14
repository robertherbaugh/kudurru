package blocking

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

type candidate struct {
	decision                            Decision
	rank, specificity, modifiers, order int
}

func better(a, b candidate) bool {
	return a.rank > b.rank || a.rank == b.rank && (a.specificity > b.specificity || a.specificity == b.specificity && (a.modifiers > b.modifiers || a.modifiers == b.modifiers && a.decision.List == b.decision.List && a.order < b.order))
}
func candidateFor(l *List, r *compiledRule, name string) candidate {
	action := ActionBlock
	reason := ReasonDeny
	if l.mode == ModeAllow {
		action, reason = ActionAllow, ReasonAllowList
	} else if r.exception {
		action, reason = ActionAllow, ReasonException
	}
	kind := parser.KindSuffix
	if strings.HasPrefix(strings.TrimPrefix(r.text, "@@"), "/") {
		kind = parser.KindRegex
	}
	return candidate{Decision{Action: action, Reason: reason, List: l.name, Match: Match{Text: r.text, Pattern: r.text, Kind: kind, Exception: r.exception}, Name: name}, r.rank(), r.specificity, r.modifiers, r.order}
}
func (sel *Selection) canceled(l *List, key string) bool {
	// Explicit allow lists are a separate policy engine. Block-list
	// cancellations cannot remove their authorization.
	return l.mode == ModeBlock && sel.badfilters[key]
}
func (sel *Selection) fastMatch(l *List, except bool, name string) (Match, bool) {
	chain := &l.primary
	if except {
		chain = &l.except
	}
	if len(sel.badfilters) == 0 || l.mode == ModeAllow {
		return chain.match(name, matcher.Hash(name))
	}
	accepts := func(kind parser.Kind, pattern string, mask uint8) (Match, bool) {
		for mask != 0 {
			syntax := firstSyntax(mask)
			mask &^= syntaxMask(syntax)
			m := Match{Kind: kind, Syntax: syntax, Pattern: pattern, Exception: except}
			// badfilter is an adblock directive, never a hosts-file edit.
			if syntax == parser.SyntaxHosts || !sel.canceled(l, m.Rule()+"$") {
				return m, true
			}
		}
		return Match{}, false
	}
	if mask, ok := chain.exact.Match(name); ok {
		if m, ok := accepts(parser.KindExact, name, mask); ok {
			return m, true
		}
	}
	var result Match
	chain.suffix.Visit(name, func(base string, tag uint8) bool {
		if m, ok := accepts(parser.KindSuffix, base, tag); ok {
			result = m
			return false
		}
		return true
	})
	if result.Kind != 0 {
		return result, true
	}
	chain.regex.Visit(name, func(src string, tag uint8) bool {
		if m, ok := accepts(parser.KindRegex, src, tag); ok {
			result = m
			return false
		}
		return true
	})
	return result, result.Kind != 0
}
func (sel *Selection) basic(l *List, ctx Context) candidate {
	best := candidate{rank: -1}
	for _, except := range []bool{false, true} {
		if m, ok := sel.fastMatch(l, except, ctx.Name); ok {
			rank := 0
			action, reason := ActionBlock, ReasonDeny
			if l.mode == ModeAllow {
				action, reason = ActionAllow, ReasonAllowList
			} else if except {
				rank, action, reason = 1, ActionAllow, ReasonException
			}
			c := candidate{Decision{Action: action, Reason: reason, List: l.name, Match: m, Name: ctx.Name}, rank, len(m.Pattern), 0, -1}
			if better(c, best) {
				best = c
			}
		}
	}
	l.slowIndex.Visit(ctx.Name, func(id int) {
		r := l.slow[id]
		if (r.rewrite != nil && l.mode != ModeAllow) || sel.canceled(l, r.key) || !r.matches(ctx) {
			return
		}
		c := candidateFor(l, r, ctx.Name)
		if better(c, best) {
			best = c
		}
	})
	return best
}

// EvaluateContext applies all modifiers against one original query and the
// pinned client identity. The fast path remains allocation-free when no list
// needs contextual rules, preserving the ordinary suffix/exact hot path.
func (sel *Selection) EvaluateContext(ctx Context) Decision {
	if !sel.contextual {
		return sel.Evaluate(ctx.Name)
	}
	ctx.Name = NormalizeName(ctx.Name)
	for _, l := range sel.allow {
		c := sel.basic(l, ctx)
		if c.rank >= 0 {
			return c.decision
		}
	}
	if sel.opts.AllowOnly {
		return Decision{Action: ActionBlock, Reason: ReasonAllowOnly, Name: ctx.Name}
	}
	if rewrite := sel.rewrites(ctx); rewrite.Action == ActionRewrite {
		return rewrite
	}
	return sel.contextBlock(ctx)
}
func (sel *Selection) contextBlock(ctx Context) Decision {
	best := candidate{rank: -1}
	var excepted Decision
	for _, l := range sel.block {
		c := sel.basic(l, ctx)
		if c.rank < 0 {
			continue
		}
		if sel.opts.ExceptionScope == ScopeSelection {
			if better(c, best) {
				best = c
			}
			continue
		}
		if c.decision.Action == ActionBlock {
			if best.rank < 0 || better(c, best) {
				best = c
			}
		} else if excepted.Action == ActionPass {
			excepted = c.decision
		}
	}
	if best.rank >= 0 {
		return best.decision
	}
	return excepted
}

func (sel *Selection) rewrites(ctx Context) Decision {
	type rewriteCandidate struct {
		list      *List
		rule      *compiledRule
		listOrder int
	}
	var rewrites, exceptions []rewriteCandidate
	for i, l := range sel.block {
		if !l.hasRewrites {
			continue
		}
		l.slowIndex.Visit(ctx.Name, func(id int) {
			r := l.slow[id]
			if r.rewrite == nil || sel.canceled(l, r.key) || !r.matches(ctx) {
				return
			}
			c := rewriteCandidate{l, r, i}
			if r.exception {
				exceptions = append(exceptions, c)
			} else {
				rewrites = append(rewrites, c)
			}
		})
	}
	slices.SortFunc(rewrites, func(a, b rewriteCandidate) int {
		if n := cmp.Compare(a.listOrder, b.listOrder); n != 0 {
			return n
		}
		return cmp.Compare(a.rule.order, b.rule.order)
	})
	var selected []rewriteCandidate
	for _, c := range rewrites {
		canceled := false
		for _, e := range exceptions {
			if sel.opts.ExceptionScope == ScopeList && e.list != c.list {
				continue
			}
			if c.rule.important && !e.rule.important {
				continue
			}
			if e.rule.rewrite.all || e.rule.rewrite.identity() == c.rule.rewrite.identity() {
				canceled = true
				break
			}
		}
		if !canceled {
			selected = append(selected, c)
		}
	}
	if len(selected) == 0 {
		return Decision{}
	}
	// Keyword/empty rcode responses first, then CNAME, then combined records.
	chosen := selected[0]
	for _, c := range selected {
		if rewritePriority(c.rule.rewrite) > rewritePriority(chosen.rule.rewrite) ||
			rewritePriority(c.rule.rewrite) == rewritePriority(chosen.rule.rewrite) && better(candidateFor(c.list, c.rule, ctx.Name), candidateFor(chosen.list, chosen.rule, ctx.Name)) {
			chosen = c
		}
	}
	r := chosen.rule.rewrite
	if len(r.records) > 0 && r.Target() == "" {
		r = &Rewrite{rcode: dns.RcodeSuccess}
		seen := map[string]bool{}
		for _, c := range selected {
			for _, rr := range c.rule.rewrite.records {
				key := rr.String()
				if !seen[key] {
					seen[key] = true
					r.records = append(r.records, rr)
				}
			}
		}
	}
	d := candidateFor(chosen.list, chosen.rule, ctx.Name).decision
	d.Action, d.Rewrite = ActionRewrite, r
	return d
}

func rewritePriority(r *Rewrite) int {
	if len(r.records) == 0 {
		return 3
	}
	if r.Target() != "" {
		return 2
	}
	return 1
}

// EvaluateResponseContext retains the query type/client selectors while
// inspecting targets/addresses from the returned raw answer, including hits.
func (sel *Selection) EvaluateResponseContext(ctx Context, names []string, addrs []netip.Addr) Decision {
	if !sel.contextual {
		return sel.EvaluateResponse(names, addrs)
	}
	for _, name := range names {
		ctx.Name = NormalizeName(name)
		allowed := false
		for _, l := range sel.allow {
			if sel.basic(l, ctx).rank >= 0 {
				allowed = true
				break
			}
		}
		if allowed {
			continue
		}
		d := sel.contextBlock(ctx)
		if d.Action == ActionBlock {
			d.Response = true
			return d
		}
	}
	// Bare address entries retain the existing list/selection exception scope;
	// client-specific name rules do not accidentally turn into IP filters.
	if len(addrs) > 0 {
		return sel.evalAddresses(ctx, addrs)
	}
	return Decision{}
}
func (sel *Selection) basicIP(l *List, ctx Context, a netip.Addr) candidate {
	best := candidate{rank: -1}
	for _, except := range []bool{false, true} {
		ch := &l.primary
		if except {
			ch = &l.except
		}
		if m, ok := ch.matchIP(a); ok && !sel.canceled(l, m.Rule()+"$") {
			rank := 0
			action, reason := ActionBlock, ReasonDeny
			if l.mode == ModeAllow {
				action, reason = ActionAllow, ReasonAllowList
			} else if except {
				rank, action, reason = 1, ActionAllow, ReasonException
			}
			c := candidate{Decision{Action: action, Reason: reason, List: l.name, Match: m, Addr: a, Response: true}, rank, len(m.Pattern), 0, -1}
			if better(c, best) {
				best = c
			}
		}
	}
	ctx.Name = a.Unmap().String()
	l.slowIndex.Visit(ctx.Name, func(id int) {
		r := l.slow[id]
		if !r.addr.IsValid() || r.rewrite != nil || sel.canceled(l, r.key) || !r.matches(ctx) {
			return
		}
		c := candidateFor(l, r, ctx.Name)
		c.decision.Response, c.decision.Addr = true, a
		if better(c, best) {
			best = c
		}
	})
	return best
}
func (sel *Selection) evalAddresses(ctx Context, addrs []netip.Addr) Decision {
	for _, a := range addrs {
		allowed := false
		for _, l := range sel.allow {
			if sel.basicIP(l, ctx, a).rank >= 0 {
				allowed = true
				break
			}
		}
		if allowed {
			continue
		}
		best := candidate{rank: -1}
		for _, l := range sel.block {
			c := sel.basicIP(l, ctx, a)
			if c.rank < 0 {
				continue
			}
			if sel.opts.ExceptionScope == ScopeSelection {
				if better(c, best) {
					best = c
				}
			} else if c.decision.Action == ActionBlock && better(c, best) {
				best = c
			}
		}
		if best.rank >= 0 && best.decision.Action == ActionBlock {
			return best.decision
		}
	}
	return Decision{}
}
