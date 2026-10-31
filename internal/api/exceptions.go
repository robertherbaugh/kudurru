package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// List exceptions: an entry of a block list switched off there
// (spec.sources[].exclude) is shown on the Allowlist, one of an allow list
// on the Denylist, as an entry of kind exception. Its switch is the
// exclusion: on means switched off in the list (the exception is in
// effect), off re-enables the list's entry; delete removes the exclusion.
// Nothing else of it can be edited.

// exceptionAction is the action whose page shows the exceptions of bl.
func exceptionAction(bl *v1alpha1.BlockList) domainlist.Action {
	if bl.Spec.Mode == "allow" {
		return domainlist.Deny
	}
	return domainlist.Allow
}

// exceptionEntry returns the exception (ns/list, source, value) as the
// set has it, or, when it is not in effect (the entry was switched back
// on), the same exception switched off. a must be the list's action.
func (s *Server) exceptionEntry(ctx context.Context, a domainlist.Action, ns, list, source, value string) (*domainlist.Entry, *domainlist.Set, error) {
	bl, err := s.getBlockList(ctx, ns, list)
	if err != nil {
		return nil, nil, err
	}
	idx, err := findSource(bl, source)
	if err != nil {
		return nil, nil, err
	}
	if exceptionAction(bl) != a {
		return nil, nil, errNotFound(fmt.Sprintf("%slist entry %q of %s/%s", a, value, ns, list))
	}
	set, err := s.domainLists().Load(ctx, ns)
	if err != nil {
		return nil, nil, storeError(err)
	}
	if e := set.FindException(ns, list, source, value); e != nil {
		return e, set, nil
	}
	src := bl.Spec.Sources[idx]
	t, _, ok := domainlist.FromRule(value, parser.Format(src.Format))
	if !ok {
		t = v1alpha1.EntryExact
	}
	return inactiveExceptionEntry(bl, a, idx, value, t), set, nil
}

func inactiveExceptionEntry(bl *v1alpha1.BlockList, a domainlist.Action, idx int, value string, t v1alpha1.EntryType) *domainlist.Entry {
	off := false
	return &domainlist.Entry{
		Kind: domainlist.KindException, Namespace: bl.Namespace, Action: a, Type: t, Value: value,
		Copies: []domainlist.Copy{{List: bl.Namespace + "/" + bl.Name, Source: blocking.SourceName(idx, bl.Spec.Sources[idx]), ManagedBy: store.ManagedBy(bl), Entry: v1alpha1.ListEntry{Value: value, Type: t, Enabled: &off}}},
	}
}

// setException switches the exception on (adds the exclusion) or off
// (removes it) and returns the entry as stored.
func (s *Server) setException(ctx context.Context, a domainlist.Action, id string, on bool) (*DomainEntry, error) {
	ns, list, source, value, _ := domainlist.ParseExceptionID(id)
	if _, _, err := s.exceptionEntry(ctx, a, ns, list, source, value); err != nil {
		return nil, err
	}
	body := SourceEntriesPatch{Enable: []string{value}}
	verb := "enable" // the list's entry: switched back on
	if on {
		body = SourceEntriesPatch{Disable: []string{value}}
		verb = "disable"
	}
	_, err := s.patchSourceEntries(ctx, &patchSourceEntriesInput{Namespace: ns, Name: list, Source: source, Body: body})
	s.recordAction(ctx, string(a)+"list.exception", auditList(ns, list), err, nil, map[string]any{"source": source, "value": value, "entry": verb})
	if err != nil {
		return nil, err
	}
	e, set, err := s.exceptionEntry(ctx, a, ns, list, source, value)
	if err != nil {
		return nil, err
	}
	v := s.domainView(e, set)
	return &v, nil
}

// createExceptions switches the entries for values off in the list
// in.List (one source, or every source that has them) and answers like
// POST /allowlist.
func (s *Server) createExceptions(ctx context.Context, a domainlist.Action, in *DomainEntriesCreate) (*domainWriteOutput, error) {
	if len(in.Groups) > 0 {
		return nil, errInvalid("groups: a list exception applies to the clients of the list's policies; leave groups empty")
	}
	ns, list := s.namespaceOr(in.Namespace), in.List
	if lns, lname, ok := strings.Cut(in.List, "/"); ok {
		ns, list = lns, lname
	}
	bl, err := s.getBlockList(ctx, ns, list)
	if err != nil {
		return nil, err
	}
	if exceptionAction(bl) != a {
		hint := "add it to the Denylist instead"
		if a == domainlist.Deny {
			hint = "add it to the Allowlist instead"
		}
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, fmt.Sprintf("BlockList %s/%s is a %s list; switching its entries off does not %s them", ns, list, bl.Spec.Mode, a), hint)
	}
	if _, _, ok := domainlist.ParseListName(list); ok {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, "the Allowlist and Denylist lists have no exceptions", "edit or switch off their entries instead")
	}
	t, err := domainlist.ParseType(in.Type)
	if err != nil {
		return nil, domainError(err)
	}
	processed := ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}
	keys := make([]string, len(in.Values))
	keyErrs := make([]error, len(in.Values))
	want := map[string]bool{}
	for i, v := range in.Values {
		if keys[i], keyErrs[i] = exceptionKey(t, v); keyErrs[i] == nil {
			want[keys[i]] = true
		}
	}
	// One parse of each source finds the rules of every value: a parse per
	// value re-read a cached body of tens of MiB up to 1000 times.
	type sourceHits struct {
		name  string
		rules map[string][]string
	}
	var sources []sourceHits
	if len(want) > 0 {
		for i := range bl.Spec.Sources {
			src := &bl.Spec.Sources[i]
			name := blocking.SourceName(i, *src)
			if len(src.Entries) > 0 || in.Source != "" && in.Source != name {
				continue
			}
			rules, err := s.sourceRules(ctx, bl, i, want)
			if err != nil {
				return nil, err
			}
			sources = append(sources, sourceHits{name, rules})
		}
	}
	type hit struct{ source, rule string }
	var hits []hit
	for i, v := range in.Values {
		if keyErrs[i] != nil {
			processed.Errors = append(processed.Errors, ProcessedItem{Item: v, Error: keyErrs[i].Error()})
			continue
		}
		found := 0
		for _, sh := range sources {
			for _, r := range sh.rules[keys[i]] {
				hits = append(hits, hit{sh.name, r})
				found++
			}
		}
		if found == 0 {
			where := "any source of " + ns + "/" + list
			if in.Source != "" {
				where = "source " + in.Source + " of " + ns + "/" + list
			}
			processed.Errors = append(processed.Errors, ProcessedItem{Item: v, Error: fmt.Sprintf("%q is not an entry of %s", v, where)})
			continue
		}
		processed.Success = append(processed.Success, ProcessedItem{Item: v})
	}
	bySource := map[string][]string{}
	var order []string
	for _, h := range hits {
		if _, ok := bySource[h.source]; !ok {
			order = append(order, h.source)
		}
		if !slices.Contains(bySource[h.source], h.rule) {
			bySource[h.source] = append(bySource[h.source], h.rule)
		}
	}
	out := &DomainWriteResult{Items: []DomainEntry{}, Processed: processed, Attached: []string{}, Skipped: []string{}}
	for _, src := range order {
		_, err := s.patchSourceEntries(ctx, &patchSourceEntriesInput{Namespace: ns, Name: list, Source: src, Body: SourceEntriesPatch{Disable: bySource[src]}})
		s.recordAction(ctx, string(a)+"list.exception", auditList(ns, list), err, nil, map[string]any{"source": src, "values": bySource[src], "entry": "disable"})
		if err != nil {
			return nil, err
		}
		for _, r := range bySource[src] {
			if e, set, err := s.exceptionEntry(ctx, a, ns, list, src, r); err == nil {
				out.Items = append(out.Items, s.domainView(e, set))
			}
		}
	}
	return &domainWriteOutput{Body: out}, nil
}

// exceptionKey is the pattern a value names (Rule.Pattern of the rules to
// switch off): the expression of a regex, else the lower-case name
// without ||, ^, *. and a trailing dot.
func exceptionKey(t v1alpha1.EntryType, v string) (string, error) {
	v = strings.TrimSpace(v)
	if t == v1alpha1.EntryRegex {
		k := blocking.ExcludeKey(v)
		if k == "" {
			return "", errors.New("empty expression")
		}
		return k, nil
	}
	k := strings.TrimSuffix(strings.ToLower(blocking.ExcludeKey(strings.TrimPrefix(v, "*."))), ".")
	if k == "" || strings.ContainsAny(k, " /|^*") {
		return "", fmt.Errorf("%q is not a domain name", v)
	}
	return k, nil
}

// sourceRules returns the rules of source idx of bl by pattern, for the
// patterns in keys.
func (s *Server) sourceRules(ctx context.Context, bl *v1alpha1.BlockList, idx int, keys map[string]bool) (map[string][]string, error) {
	src := &bl.Spec.Sources[idx]
	out := map[string][]string{}
	n := 0
	collect := func(r parser.Rule) error {
		if n++; n%scanCheck == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if keys[r.Pattern] {
			// The parser borrows Pattern from its read buffer; retained map
			// keys must survive the next rule callback.
			pattern := strings.Clone(r.Pattern)
			if v := r.String(); !slices.Contains(out[pattern], v) {
				out[pattern] = append(out[pattern], v)
			}
		}
		return nil
	}
	opts := parser.Options{Format: parser.Format(src.Format), HostsSubdomains: src.Subdomains, KeepInvalid: -1, KeepUnsupported: -1}
	if src.URL == "" && src.ConfigMapRef == nil {
		_, err := parser.Parse(strings.NewReader(strings.Join(src.Inline, "\n")), opts, collect)
		return out, err
	}
	name := blocking.SourceName(idx, *src)
	if s.opts.ListBodies == nil {
		return nil, errNoBody(name)
	}
	release, err := s.acquireBodyScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rc, _, err := s.opts.ListBodies.OpenSourceBody(bl.Namespace, *src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errNoBody(name)
	case err != nil:
		return nil, errUnavailable("source %q: %v", name, err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := parser.Parse(rc, opts, collect); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errUnavailable("source %q: %v", name, err)
	}
	return out, nil
}

func auditList(ns, name string) audit.Resource {
	return audit.Resource{Kind: "BlockList", Namespace: ns, Name: name}
}
