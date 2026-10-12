package domainlist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Options configure a Service.
type Options struct {
	// Store holds the lists and policies; required.
	Store store.Store
	// Now is the clock; default time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service reads and writes the allow and deny entries.
type Service struct {
	opts Options
}

// New returns a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{opts: opts}
}

func (s *Service) now() *metav1.Time {
	t := metav1.NewTime(s.opts.Now().UTC().Truncate(time.Second))
	return &t
}

// writableOwner reports whether the API may change an object of owner
// (decision D84: its own objects and the first-boot defaults, which it
// takes over).
func writableOwner(owner string) bool {
	switch owner {
	case "", store.ManagerAPI, store.ManagerBootstrap:
		return true
	}
	return false
}

func ptrFalse() *bool { b := false; return &b }

func sourceName(i int, src v1alpha1.ListSource) string { return blocking.SourceName(i, src) }

// each lists every object of kind in ns ("" for all namespaces).
func (s *Service) each(ctx context.Context, kind store.Kind, ns string, fn func(store.Object)) error {
	cont := ""
	for {
		page, err := s.opts.Store.List(ctx, kind, store.ListOptions{Namespace: ns, Limit: 500, Continue: cont})
		if err != nil {
			return err
		}
		for _, o := range page.Items {
			fn(o)
		}
		if page.Continue == "" {
			return nil
		}
		cont = page.Continue
	}
}

// Scope is where an entry applies: everyone (no groups), or the clients
// of the named ClientGroups (names in the entry's namespace).
type Scope struct {
	Groups []string
}

// Everyone reports whether the scope is everyone.
func (sc Scope) Everyone() bool { return len(sc.Groups) == 0 }

// ItemError is a value the write refused.
type ItemError struct {
	Item string
	Err  error
}

// Result is what a write changed.
type Result struct {
	// IDs are the logical entries written (after the write).
	IDs []string
	// New are the rules stored for the first time in a list.
	New []string
	// Errors are values refused one by one.
	Errors []ItemError
	// Attached are the policies ("namespace/name") that reference the
	// written lists; Skipped those that should but are managed elsewhere.
	Attached, Skipped []string
	// Names are the domain names to flush from response caches.
	Names []string
	// Lists are the BlockLists written, "namespace/name".
	Lists []string
}

func (r *Result) addName(t v1alpha1.EntryType, value string) {
	if n := MatchName(t, value); n != "" && !slices.Contains(r.Names, n) {
		r.Names = append(r.Names, n)
	}
}

func (r *Result) addID(id string) {
	if !slices.Contains(r.IDs, id) {
		r.IDs = append(r.IDs, id)
	}
}

func (r *Result) attachedTo(p string, ok bool) {
	dst := &r.Skipped
	if ok {
		dst = &r.Attached
	}
	if !slices.Contains(*dst, p) {
		*dst = append(*dst, p)
	}
}

// target is one list a scope writes to.
type target struct {
	list  string // BlockList name
	group string // ClientGroup name, "" for everyone
}

// targets resolves a scope in ns: the group lists, each group checked to
// exist and to be covered by a policy, or the everyone list.
func (s *Service) targets(ctx context.Context, a Action, ns string, sc Scope) ([]target, error) {
	if sc.Everyone() {
		return []target{{list: ListName(a, "")}}, nil
	}
	var out []target
	for _, g := range sc.Groups {
		name := g
		if gns, gname, ok := strings.Cut(g, "/"); ok {
			if gns != ns {
				return nil, fmt.Errorf("%w: client group %s is not in namespace %s", ErrInvalid, g, ns)
			}
			name = gname
		}
		if name == "" {
			return nil, fmt.Errorf("%w: empty client group", ErrInvalid)
		}
		cg := &v1alpha1.ClientGroup{}
		if err := s.opts.Store.Get(ctx, store.Key{Kind: "ClientGroup", Namespace: ns, Name: name}, cg); errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: no client group %s/%s", ErrInvalid, ns, name)
		} else if err != nil {
			return nil, err
		}
		pols, err := s.policiesFor(ctx, ns, name)
		if err != nil {
			return nil, err
		}
		if len(pols) == 0 {
			return nil, fmt.Errorf("%w %s/%s; create a policy for the group or use everyone", ErrNoPolicy, ns, name)
		}
		if !slices.ContainsFunc(out, func(t target) bool { return t.group == name }) {
			out = append(out, target{list: ListName(a, name), group: name})
		}
	}
	return out, nil
}

// policiesFor returns the policies of ns a list for group applies
// through: those whose clientGroupRefs name group, or every policy of the
// namespace for the everyone list (group "").
func (s *Service) policiesFor(ctx context.Context, ns, group string) ([]*v1alpha1.Policy, error) {
	var out []*v1alpha1.Policy
	err := s.each(ctx, "Policy", ns, func(o store.Object) {
		if p, ok := o.(*v1alpha1.Policy); ok && (group == "" || slices.Contains(p.Spec.ClientGroupRefs, group)) {
			out = append(out, p)
		}
	})
	slices.SortFunc(out, func(a, b *v1alpha1.Policy) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// AddRequest adds values of one type to the lists of a scope.
type AddRequest struct {
	Action    Action
	Namespace string
	Type      v1alpha1.EntryType
	Values    []string
	Scope     Scope
	// Comment and Enabled apply to new entries; on an entry that exists
	// they replace its comment (when not empty) and its switch (when set).
	Comment string
	Enabled *bool
	Origin  v1alpha1.EntryOrigin
	// Request is the UnblockRequest ("namespace/name") of an approval.
	Request string
	// Actor is who adds ("user:alice").
	Actor string
}

// Add stores the values in every list of the scope, creating the lists
// and attaching them to the policies concerned. Values that do not
// validate are reported in Result.Errors; a scope error fails the whole
// request.
func (s *Service) Add(ctx context.Context, req AddRequest) (*Result, error) {
	if req.Origin == "" {
		req.Origin = v1alpha1.OriginManual
	}
	if len(req.Comment) > 500 {
		return nil, fmt.Errorf("%w: comment longer than 500 characters", ErrInvalid)
	}
	targets, err := s.targets(ctx, req.Action, req.Namespace, req.Scope)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	var values []string
	for _, v := range req.Values {
		c, err := Normalize(req.Type, v)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{Item: v, Err: err})
			continue
		}
		if !slices.Contains(values, c) {
			values = append(values, c)
		}
	}
	if len(values) == 0 {
		return res, nil
	}
	now := s.now()
	for _, tg := range targets {
		_, err := s.mutate(ctx, req.Namespace, tg.list, req.Action, true, func(entries []v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool) {
			changed := false
			for _, v := range values {
				i := slices.IndexFunc(entries, func(e v1alpha1.ListEntry) bool { return e.Value == v && typeOf(e) == req.Type })
				if i >= 0 {
					e := &entries[i]
					if req.Comment != "" && e.Comment != req.Comment {
						e.Comment, changed = req.Comment, true
					}
					if req.Enabled != nil && e.IsEnabled() != *req.Enabled {
						e.Enabled, changed = enabledPtr(*req.Enabled), true
					}
					if changed {
						e.Modified = now
					}
					continue
				}
				entries = append(entries, v1alpha1.ListEntry{
					Value: v, Type: req.Type, Comment: req.Comment, Created: now, CreatedBy: req.Actor,
					Origin: req.Origin, Request: req.Request, Enabled: enabledPtr(req.Enabled == nil || *req.Enabled),
				})
				res.New = append(res.New, v1alpha1.EntryRule(req.Type, v))
				changed = true
			}
			return entries, changed
		})
		if err != nil {
			return nil, err
		}
		res.Lists = append(res.Lists, req.Namespace+"/"+tg.list)
		if err := s.attachTarget(ctx, req.Action, req.Namespace, tg, res); err != nil {
			return nil, err
		}
	}
	for _, v := range values {
		res.addID(ID(req.Namespace, req.Type, v))
		res.addName(req.Type, v)
	}
	return res, nil
}

// enabledPtr is nil for true (the default) and a false pointer otherwise.
func enabledPtr(on bool) *bool {
	if on {
		return nil
	}
	return ptrFalse()
}

func typeOf(e v1alpha1.ListEntry) v1alpha1.EntryType {
	if e.Type == "" {
		return v1alpha1.EntryExact
	}
	return e.Type
}

// Patch changes one logical entry; nil fields stay.
type Patch struct {
	Enabled *bool
	Comment *string
	Type    *v1alpha1.EntryType
	Value   *string
	// Scope replaces where the entry applies.
	Scope *Scope
	Actor string
}

// Update applies p to the entry (namespace, type, value) of action in
// every list that holds it; a new scope moves it between lists.
func (s *Service) Update(ctx context.Context, a Action, ns string, t v1alpha1.EntryType, value string, p Patch) (*Result, error) {
	set, err := s.Load(ctx, ns)
	if err != nil {
		return nil, err
	}
	e := set.Find(a, ns, t, value)
	if e == nil {
		return nil, fmt.Errorf("%w: %s %s %q in namespace %s", ErrNotFound, a, t, value, ns)
	}
	if owner := e.ReadOnly(); owner != "" {
		c := e.Copies[0]
		for _, cp := range e.Copies {
			if !writableOwner(cp.ManagedBy) {
				c = cp
			}
		}
		lns, lname, _ := strings.Cut(c.List, "/")
		return nil, &store.ErrManagedBy{Key: store.Key{Kind: "BlockList", Namespace: lns, Name: lname}, Owner: owner}
	}
	newType, newValue := t, value
	if p.Type != nil {
		newType = *p.Type
	}
	if p.Value != nil || p.Type != nil {
		v := value
		if p.Value != nil {
			v = *p.Value
		}
		if newValue, err = Normalize(newType, v); err != nil {
			return nil, err
		}
	}
	if p.Comment != nil && len(*p.Comment) > 500 {
		return nil, fmt.Errorf("%w: comment longer than 500 characters", ErrInvalid)
	}
	if newType != t || newValue != value {
		if other := set.Find(a, ns, newType, newValue); other != nil {
			return nil, fmt.Errorf("%w: %s %s %q exists already; edit or delete that entry", ErrInvalid, a, newType, newValue)
		}
	}
	// The lists the entry lives in now, and the lists of the new scope.
	var old []target
	for _, c := range e.Copies {
		_, lname, _ := strings.Cut(c.List, "/")
		_, g, _ := strings.Cut(c.Group, "/")
		old = append(old, target{list: lname, group: g})
	}
	next := old
	if p.Scope != nil {
		if next, err = s.targets(ctx, a, ns, *p.Scope); err != nil {
			return nil, err
		}
	}
	base := *e.Primary()
	base.Type, base.Value, base.Modified = newType, newValue, s.now()
	if p.Comment != nil {
		base.Comment = *p.Comment
	}
	if p.Enabled != nil {
		base.Enabled = enabledPtr(*p.Enabled)
	} else {
		base.Enabled = enabledPtr(e.Enabled())
	}
	if base.Origin == "" {
		base.Origin = v1alpha1.OriginManual
	}
	res := &Result{}
	all := slices.Clone(next)
	for _, o := range old {
		if !slices.Contains(all, o) {
			all = append(all, o)
		}
	}
	for _, tg := range all {
		keep := slices.Contains(next, tg)
		_, err := s.mutate(ctx, ns, tg.list, a, keep, func(entries []v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool) {
			before := len(entries)
			entries = slices.DeleteFunc(entries, func(x v1alpha1.ListEntry) bool {
				return (x.Value == value && typeOf(x) == t) || (x.Value == newValue && typeOf(x) == newType)
			})
			if keep {
				entries = append(entries, base)
				return entries, true
			}
			return entries, len(entries) != before
		})
		if err != nil {
			return nil, err
		}
		res.Lists = append(res.Lists, ns+"/"+tg.list)
		if keep {
			if err := s.attachTarget(ctx, a, ns, tg, res); err != nil {
				return nil, err
			}
		}
	}
	res.addID(ID(ns, newType, newValue))
	res.addName(t, value)
	res.addName(newType, newValue)
	return res, nil
}

// Delete removes the entry (namespace, type, value) of action from every
// list that holds it.
func (s *Service) Delete(ctx context.Context, a Action, ns string, t v1alpha1.EntryType, value string) (*Result, error) {
	set, err := s.Load(ctx, ns)
	if err != nil {
		return nil, err
	}
	e := set.Find(a, ns, t, value)
	if e == nil {
		return nil, fmt.Errorf("%w: %s %s %q in namespace %s", ErrNotFound, a, t, value, ns)
	}
	res := &Result{}
	for _, c := range e.Copies {
		_, lname, _ := strings.Cut(c.List, "/")
		if _, err := s.mutate(ctx, ns, lname, a, false, func(entries []v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool) {
			n := len(entries)
			entries = slices.DeleteFunc(entries, func(x v1alpha1.ListEntry) bool { return x.Value == value && typeOf(x) == t })
			return entries, len(entries) != n
		}); err != nil {
			return nil, err
		}
		if !slices.Contains(res.Lists, c.List) {
			res.Lists = append(res.Lists, c.List)
		}
	}
	res.addName(t, value)
	return res, nil
}

// RemoveRule takes the entry whose rule is rule ("example.com",
// "*.example.com", or a legacy "||example.com^") off the list ns/name,
// when its origin is origin ("" for any); the list is deleted and
// detached once it holds nothing else. It is how an unblock approval
// releases its entry (origin unblock), so an entry someone added by hand
// with the same rule meanwhile stays.
func (s *Service) RemoveRule(ctx context.Context, ns, name, rule string, origin v1alpha1.EntryOrigin) error {
	a, _, ok := ParseListName(name)
	if !ok {
		a = Allow
	}
	key := RuleKey(rule)
	_, err := s.mutate(ctx, ns, name, a, false, func(entries []v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool) {
		n := len(entries)
		entries = slices.DeleteFunc(entries, func(x v1alpha1.ListEntry) bool {
			return RuleKey(v1alpha1.EntryRule(typeOf(x), x.Value)) == key && (origin == "" || x.Origin == origin)
		})
		return entries, len(entries) != n
	})
	return err
}

// AddToList stores values in the list ns/name, which need not be a ui
// list (POST /domains with a list name), and references it from the
// default policy when kudurru may edit that policy.
func (s *Service) AddToList(ctx context.Context, req AddRequest, name string) (*Result, error) {
	if req.Origin == "" {
		req.Origin = v1alpha1.OriginAPI
	}
	res := &Result{}
	var values []string
	for _, v := range req.Values {
		c, err := Normalize(req.Type, v)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{Item: v, Err: err})
			continue
		}
		values = append(values, c)
	}
	if len(values) == 0 {
		return res, nil
	}
	now := s.now()
	if _, err := s.mutate(ctx, req.Namespace, name, req.Action, true, func(entries []v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool) {
		changed := false
		for _, v := range values {
			if slices.ContainsFunc(entries, func(e v1alpha1.ListEntry) bool { return e.Value == v && typeOf(e) == req.Type }) {
				continue
			}
			entries = append(entries, v1alpha1.ListEntry{Value: v, Type: req.Type, Created: now, CreatedBy: req.Actor, Origin: req.Origin})
			res.New = append(res.New, v1alpha1.EntryRule(req.Type, v))
			changed = true
		}
		return entries, changed
	}); err != nil {
		return nil, err
	}
	res.Lists = append(res.Lists, req.Namespace+"/"+name)
	pol := &v1alpha1.Policy{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "Policy", Namespace: req.Namespace, Name: "default"}, pol); err == nil {
		ok, err := s.attach(ctx, req.Action, pol, name)
		if err != nil {
			return nil, err
		}
		res.attachedTo(req.Namespace+"/default", ok)
	}
	for _, v := range values {
		res.addID(ID(req.Namespace, req.Type, v))
		res.addName(req.Type, v)
	}
	return res, nil
}
