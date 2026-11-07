package blocking

import (
	"net/netip"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// ExceptionScope says which deny rules a @@ exception can cancel.
type ExceptionScope uint8

// Exception scopes.
const (
	// ScopeList: a @@ rule cancels deny rules of its own list only. This is
	// the default: a list's exceptions fix that list's false positives and
	// cannot silently unblock another list (use an allow-mode list for
	// that).
	ScopeList ExceptionScope = iota
	// ScopeSelection: a @@ rule in any block list of the selection cancels
	// deny rules of every list, matching AdGuard Home, which merges all
	// filter lists into one engine.
	ScopeSelection
)

// SelectOptions are the per-policy evaluation options.
type SelectOptions struct {
	// AllowOnly blocks every name no allow-mode list matches
	// (Policy.allowOnly, blocky's allowlist-only groups).
	AllowOnly bool
	// ExceptionScope defaults to ScopeList.
	ExceptionScope ExceptionScope
}

// Selection is the set of lists one policy applies, resolved against one
// snapshot. It is immutable and safe for concurrent use.
type Selection struct {
	block, allow []*List
	opts         SelectOptions
	contextual   bool
	badfilters   map[string]bool
}

// BlockLists returns the names of the block-mode lists in evaluation order.
func (sel *Selection) BlockLists() []string { return names(sel.block) }

// AllowLists returns the names of the allow-mode lists in evaluation order.
func (sel *Selection) AllowLists() []string { return names(sel.allow) }

// Options returns the evaluation options.
func (sel *Selection) Options() SelectOptions { return sel.opts }

func names(ls []*List) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.name
	}
	return slices.Clip(out)
}

// Action is the outcome of an evaluation.
type Action uint8

// Actions.
const (
	// ActionPass: no list decided; continue down the pipeline.
	ActionPass Action = iota
	// ActionAllow: an allow-mode list or an exception explicitly allowed
	// the name; response-side checks are skipped for an allow-list match.
	ActionAllow
	// ActionBlock: synthesise the policy's block response.
	ActionBlock
	// ActionRewrite is an explicit typed replacement, never a raw-cache entry.
	ActionRewrite
)

func (a Action) String() string {
	switch a {
	case ActionPass:
		return "pass"
	case ActionAllow:
		return "allow"
	case ActionBlock:
		return "block"
	case ActionRewrite:
		return "rewrite"
	}
	return "unknown"
}

// Reason says which rule of the decision order decided.
type Reason uint8

// Reasons.
const (
	ReasonNone Reason = iota
	// ReasonAllowList: an allow-mode list matched.
	ReasonAllowList
	// ReasonAllowOnly: allow-only mode and no allow-mode list matched.
	ReasonAllowOnly
	// ReasonException: a deny rule matched but a @@ exception cancelled it.
	ReasonException
	// ReasonDeny: a deny rule matched.
	ReasonDeny
)

func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "none"
	case ReasonAllowList:
		return "allowlist"
	case ReasonAllowOnly:
		return "allow_only"
	case ReasonException:
		return "exception"
	case ReasonDeny:
		return "deny"
	}
	return "unknown"
}

// Decision is the result of evaluating a name. The zero value is "pass".
// Its strings point into the snapshot, so a Decision costs no allocation.
type Decision struct {
	Rewrite *Rewrite
	Action  Action
	Reason  Reason
	// List is the deciding list; empty for ReasonAllowOnly.
	List string
	// Match is the deciding rule; zero for ReasonAllowOnly.
	Match Match
	// Name is the normalised name that decided: the question, or the CNAME
	// target for a response-side block.
	Name string
	// Addr is the answer address that decided a response-side block.
	Addr netip.Addr
	// Response is true when an answer record, not the question, decided.
	Response bool
}

// Blocked reports whether the decision is to block.
func (d Decision) Blocked() bool { return d.Action == ActionBlock }

// Rule renders the deciding rule for the query log "why" row
// (pipeline.Response.Rule); empty for allow-only blocks and passes.
func (d Decision) Rule() string { return d.Match.Rule() }

// Status maps a block decision onto the query-log status taxonomy
// (dev-docs/design-record.md §11.2): response-side blocks are blocked_cname,
// regex rules blocked_regex, everything else blocked_list. Non-blocking
// decisions return StatusUnknown; the stage keeps whatever status the rest
// of the pipeline produces. blocked_exact is left to the stage, which knows
// whether a list is the user's own inline list.
func (d Decision) Status() pipeline.Status {
	switch {
	case d.Action != ActionBlock:
		return pipeline.StatusUnknown
	case d.Response:
		return pipeline.StatusBlockedCNAME
	case d.Match.Kind == parser.KindRegex:
		return pipeline.StatusBlockedRegex
	}
	return pipeline.StatusBlockedList
}

// Evaluate decides a question name: allow lists win, then allow-only, then
// each block list's deny rules unless an exception cancels them. It never
// allocates for an already lower-case name.
func (sel *Selection) Evaluate(qname string) Decision {
	if sel.contextual {
		return sel.EvaluateContext(Context{Name: qname, Type: 1, Class: 1})
	}
	n := NormalizeName(qname)
	h := matcher.Hash(n)
	for _, l := range sel.allow {
		if m, ok := l.primary.match(n, h); ok {
			return Decision{Action: ActionAllow, Reason: ReasonAllowList, List: l.name, Match: m, Name: n}
		}
	}
	if sel.opts.AllowOnly {
		return Decision{Action: ActionBlock, Reason: ReasonAllowOnly, Name: n}
	}
	return sel.evalBlock(n, h)
}

// EvaluateResponse applies the response-side checks after the upstream
// answered: every CNAME target in names (and SVCB/HTTPS target names) and
// every A/AAAA address or address hint in addrs is checked against the
// allow lists first and then the block lists, as Evaluate does for the
// question. The first block wins and has Response set. Allow-only mode does
// not apply: the question was already allowed. The stage skips this call
// when Evaluate returned ReasonAllowList for the question.
func (sel *Selection) EvaluateResponse(names []string, addrs []netip.Addr) Decision {
	if sel.contextual {
		return sel.EvaluateResponseContext(Context{Type: 1, Class: 1}, names, addrs)
	}
nameLoop:
	for _, raw := range names {
		n := NormalizeName(raw)
		h := matcher.Hash(n)
		for _, l := range sel.allow {
			if _, ok := l.primary.match(n, h); ok {
				continue nameLoop
			}
		}
		if d := sel.evalBlock(n, h); d.Action == ActionBlock {
			d.Response = true
			return d
		}
	}
addrLoop:
	for _, a := range addrs {
		for _, l := range sel.allow {
			if _, ok := l.primary.matchIP(a); ok {
				continue addrLoop
			}
		}
		if d := sel.evalBlockIP(a); d.Action == ActionBlock {
			return d
		}
	}
	return Decision{}
}

// evalBlock runs the block lists for a normalised name. Exceptions are only
// consulted after a deny match, so the common miss costs one lookup per
// list.
func (sel *Selection) evalBlock(n string, h uint64) Decision {
	var excepted Decision
	for _, l := range sel.block {
		m, ok := l.primary.match(n, h)
		if !ok {
			continue
		}
		deny := Decision{Action: ActionBlock, Reason: ReasonDeny, List: l.name, Match: m, Name: n}
		if sel.opts.ExceptionScope == ScopeSelection {
			for _, el := range sel.block {
				if em, eok := el.except.match(n, h); eok {
					return Decision{Action: ActionAllow, Reason: ReasonException, List: el.name, Match: em, Name: n}
				}
			}
			return deny
		}
		if em, eok := l.except.match(n, h); eok {
			if excepted.Action == ActionPass {
				excepted = Decision{Action: ActionAllow, Reason: ReasonException, List: l.name, Match: em, Name: n}
			}
			continue
		}
		return deny
	}
	return excepted
}

func (sel *Selection) evalBlockIP(a netip.Addr) Decision {
	var excepted Decision
	for _, l := range sel.block {
		m, ok := l.primary.matchIP(a)
		if !ok {
			continue
		}
		deny := Decision{Action: ActionBlock, Reason: ReasonDeny, List: l.name, Match: m, Addr: a, Response: true}
		if sel.opts.ExceptionScope == ScopeSelection {
			for _, el := range sel.block {
				if em, eok := el.except.matchIP(a); eok {
					return Decision{Action: ActionAllow, Reason: ReasonException, List: el.name, Match: em, Addr: a, Response: true}
				}
			}
			return deny
		}
		if em, eok := l.except.matchIP(a); eok {
			if excepted.Action == ActionPass {
				excepted = Decision{Action: ActionAllow, Reason: ReasonException, List: l.name, Match: em, Addr: a, Response: true}
			}
			continue
		}
		return deny
	}
	return excepted
}
