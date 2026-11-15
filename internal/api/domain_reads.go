package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func (s *Server) capturedDomainSet(ctx context.Context, ns string) (*domainlist.Set, zonecache.ObjectSet, serving.Identity, error) {
	id, err := s.convenienceIdentity(ctx)
	if err != nil {
		return nil, zonecache.ObjectSet{}, id, err
	}
	objects, base, err := s.capturedObjects(ctx)
	if err != nil || !base.Same(id.Base) || objects.Lists() == nil {
		return nil, zonecache.ObjectSet{}, id, objectReadUnavailable()
	}
	// Reuse the canonical projector over detached captured objects. The editor
	// is local to this read and has no Store, publisher, settings or write hook.
	var input []store.Object
	total := 0
	for _, kind := range []store.Kind{"BlockList", "Policy", "ClientGroup"} {
		for _, object := range objects.List(kind) {
			if ns != "" && object.GetNamespace() != ns {
				continue
			}
			if ctx.Err() != nil || len(input) >= maxDomainCommandObjects {
				return nil, zonecache.ObjectSet{}, id, objectReadUnavailable()
			}
			data, err := json.Marshal(object)
			if err != nil || len(data) > MaxObjectBytes || len(data) > maxDomainCommandBytes-total {
				return nil, zonecache.ObjectSet{}, id, objectReadUnavailable()
			}
			total += len(data)
			input = append(input, object)
		}
	}
	set, err := domainlist.New(domainlist.Options{Store: domainlist.NewPlan(input)}).Load(ctx, ns)
	if err != nil {
		return nil, zonecache.ObjectSet{}, id, objectReadUnavailable()
	}
	return set, objects, id, nil
}

func (s *Server) listCapturedDomainEntries(ctx context.Context, actions []domainlist.Action, in *domainListInput) (*domainListOutput, error) {
	for _, value := range []string{in.Namespace, in.Q, in.Scope, in.Kind, in.Type, in.Enabled} {
		if !boundedSourceString(value, 4096) {
			return nil, errInvalid("domain query is too large or invalid UTF-8")
		}
	}
	set, objects, cut, err := s.capturedDomainSet(ctx, in.Namespace)
	if err != nil {
		return nil, err
	}
	queryInput := *in
	queryInput.Continue = ""
	queryInput.Q = strings.ToLower(strings.TrimSpace(in.Q))
	queryInput.Scope = strings.TrimSpace(in.Scope)
	queryInput.Limit = clampLimit(in.Limit, 100, 1000)
	query := convenienceQuery(struct {
		Actions []domainlist.Action
		Input   domainListInput
	}{actions, queryInput})
	start, err := conveniencePageStart(in.Continue, cut, query)
	if err != nil {
		return nil, err
	}
	out, err := s.domainEntriesPage(ctx, actions, in, set, start, objects.Lists(), func(offset int) string {
		return conveniencePageNext(cut, query, offset)
	})
	if err != nil || ctx.Err() != nil {
		return nil, objectReadUnavailable()
	}
	return out, nil
}

func (s *Server) getCapturedDomainEntry(ctx context.Context, a domainlist.Action, id string) (*domainEntryOutput, error) {
	if !boundedSourceString(id, maxObjectReadCursor) {
		return nil, errInvalid("domain entry ID is too large or invalid UTF-8")
	}
	ns, list, source, value, exception := domainlist.ParseExceptionID(id)
	var t public.EntryType
	var err error
	if !exception {
		ns, t, value, err = domainlist.ParseID(id)
		if err != nil {
			return nil, domainError(err)
		}
	}
	set, objects, _, err := s.capturedDomainSet(ctx, ns)
	if err != nil {
		return nil, err
	}
	var e *domainlist.Entry
	if exception {
		e, err = s.capturedExceptionEntry(ctx, a, ns, list, source, value, set, objects)
		if err != nil {
			return nil, err
		}
	} else {
		e = set.Find(a, ns, t, value)
	}
	if e == nil {
		return nil, errNotFound(fmt.Sprintf("%slist entry %q in namespace %s", a, value, ns))
	}
	v := s.domainViewWithLists(e, set, objects.Lists())
	if ctx.Err() != nil {
		return nil, objectReadUnavailable()
	}
	return &domainEntryOutput{Body: &v}, nil
}

func (s *Server) capturedExceptionEntry(ctx context.Context, a domainlist.Action, ns, list, source, value string, set *domainlist.Set, objects zonecache.ObjectSet) (*domainlist.Entry, error) {
	o, found := objects.Get(store.Key{Kind: "BlockList", Namespace: ns, Name: list})
	if !found {
		return nil, errNotFound(ns + "/" + list)
	}
	bl, ok := o.(*public.BlockList)
	if !ok || exceptionAction(bl) != a {
		return nil, errNotFound("exception of " + ns + "/" + list)
	}
	idx, err := findSource(bl, source)
	if err != nil {
		return nil, err
	}
	src := bl.Spec.Sources[idx]
	name := blocking.SourceName(idx, src)
	if _, _, ui := domainlist.ParseListName(bl.Name); ui || len(src.Entries) > 0 || blocking.IsRPZSource(src) {
		return nil, errNotFound("exception of " + ns + "/" + list)
	}
	// A switched-off exception still reports the policies and manager of the
	// original list, even when no exclusions remain on that list.
	key := ns + "/" + list
	set.ExceptionPolicies[key] = nil
	for _, p := range objects.Policies() {
		if p.Namespace == ns && (slices.Contains(p.Spec.AllowListRefs, list) || slices.Contains(p.Spec.BlockListRefs, list)) {
			set.ExceptionPolicies[key] = append(set.ExceptionPolicies[key], ns+"/"+p.Name)
		}
	}
	slices.Sort(set.ExceptionPolicies[key])
	if e := set.FindException(ns, list, name, value); e != nil {
		return e, nil
	}
	// An inactive exception's ID must still name an actual original source
	// rule. External rules use the exact captured receipt, never ListBodies.
	t, _, valid := domainlist.FromRule(value, parser.Format(src.Format))
	if !valid {
		return nil, errNotFound("source rule " + value)
	}
	pattern := blocking.ExcludeKey(value)
	rules, err := s.commandExceptionRules(ctx, bl, idx, map[string]bool{pattern: true})
	if err != nil {
		return nil, sourceReadUnavailable()
	}
	if !slices.ContainsFunc(rules[pattern], func(rule string) bool { return blocking.ExcludeKey(rule) == pattern }) {
		return nil, errNotFound("source rule " + value)
	}
	return inactiveExceptionEntry(bl, a, idx, value, t), nil
}
