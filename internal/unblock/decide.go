package unblock

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Allow lists: approvals for everyone go to the UI allow list (the
// Domains page's, POST /domains/allow), approvals for one ClientGroup to
// ui-allow-<group> in the group's namespace. Both keep their entries in
// the inline source "ui".
const (
	EveryoneList    = "ui-allow"
	GroupListPrefix = "ui-allow-"
	uiSource        = "ui"
	// DefaultPolicy is the fallback policy every everyone-approval
	// attaches the list to.
	DefaultPolicy = "default"
)

// Scopes and matches of an approval.
const (
	ScopeEveryone   = "everyone"
	ScopeGroup      = "group"
	MatchExact      = "exact"
	MatchSubdomains = "subdomains"
)

// Decision is an administrator's approval or denial.
type Decision struct {
	// Scope is everyone or group; Match exact or subdomains.
	Scope, Match string
	// Group is the ClientGroup of a group approval, "ns/name" or a name in
	// the request's namespace; empty takes the request's first group.
	Group string
	// Duration limits the approval; 0 is permanent.
	Duration time.Duration
	// Note is shown to the user on the block page.
	Note string
	// Actor is who decided ("user:alice").
	Actor string
}

// Outcome is what an approval changed.
type Outcome struct {
	Request *v1alpha1.UnblockRequest
	// Attached are the policies the list was added to; Skipped those that
	// should have it but are managed elsewhere (file, integration).
	Attached, Skipped []string
}

// load reads a request from the store.
func (s *Service) load(ctx context.Context, ns, name string) (*v1alpha1.UnblockRequest, error) {
	r := &v1alpha1.UnblockRequest{}
	if err := s.opts.Store.Get(ctx, keyOf(ns, name), r); err != nil {
		return nil, err
	}
	return r, nil
}

// Entry is the allow-list line of domain for match.
func Entry(domain, match string) string {
	if match == MatchSubdomains {
		return "||" + domain + "^"
	}
	return domain
}

// Approve adds the allow entry of d for the request ns/name and records
// the approval. A request already approved answers ErrState (deny it to
// revoke the approval first).
func (s *Service) Approve(ctx context.Context, ns, name string, d Decision) (*Outcome, error) {
	r, err := s.load(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if Active(r, now) {
		return nil, fmt.Errorf("%w: %s/%s is already approved; deny it to revoke the approval", ErrState, ns, name)
	}
	switch d.Match {
	case "":
		d.Match = MatchExact
	case MatchExact, MatchSubdomains:
	default:
		return nil, fmt.Errorf("%w: match %q (exact or subdomains)", ErrInvalid, d.Match)
	}
	if d.Duration < 0 {
		return nil, fmt.Errorf("%w: duration must not be negative", ErrInvalid)
	}
	listNS, list, group := ns, EveryoneList, ""
	var policies []string // names in listNS
	switch d.Scope {
	case ScopeEveryone, "":
		d.Scope = ScopeEveryone
		policies = []string{DefaultPolicy}
		for _, p := range r.Spec.Policies {
			if pns, pname, ok := strings.Cut(p, "/"); ok && pns == listNS && !slices.Contains(policies, pname) {
				policies = append(policies, pname)
			}
		}
	case ScopeGroup:
		group = d.Group
		if group == "" {
			if len(r.Spec.Groups) == 0 {
				return nil, fmt.Errorf("%w: the request names no client group; approve it for everyone", ErrInvalid)
			}
			group = r.Spec.Groups[0]
		}
		if !strings.Contains(group, "/") {
			group = ns + "/" + group
		}
		gns, gname, _ := strings.Cut(group, "/")
		listNS, list = gns, GroupListPrefix+gname
		if policies, err = s.policiesOfGroup(ctx, gns, gname); err != nil {
			return nil, err
		}
		if len(policies) == 0 {
			return nil, fmt.Errorf("%w: no policy covers client group %s; approve it for everyone", ErrInvalid, group)
		}
	default:
		return nil, fmt.Errorf("%w: scope %q (everyone or group)", ErrInvalid, d.Scope)
	}
	// An ended time-limited approval the janitor has not processed yet
	// gives up its entry first, so the new one owns it.
	if err := s.end(ctx, r, now); err != nil {
		return nil, err
	}
	entry := Entry(r.Spec.Domain, d.Match)
	added, err := s.addEntry(ctx, listNS, list, entry)
	if err != nil {
		return nil, err
	}
	out := &Outcome{}
	for _, p := range policies {
		ok, err := s.attach(ctx, listNS, p, list)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, err
		case ok:
			out.Attached = append(out.Attached, listNS+"/"+p)
		default:
			out.Skipped = append(out.Skipped, listNS+"/"+p)
		}
	}
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	allow := &v1alpha1.UnblockAllow{Scope: d.Scope, Match: d.Match, Group: group, List: listNS + "/" + list, Entry: entry, Added: added, Policies: out.Attached}
	if d.Duration > 0 {
		exp := metav1.NewTime(now.Add(d.Duration).UTC().Truncate(time.Second))
		allow.ExpiresAt = &exp
	}
	st := v1alpha1.UnblockRequestStatus{
		State: v1alpha1.UnblockApproved, DecidedBy: d.Actor, DecidedAt: &at, Note: d.Note, Allow: allow,
		History: pushHistory(r.Status),
	}
	if out.Request, err = s.setStatus(ctx, r, st); err != nil {
		return nil, err
	}
	return out, nil
}

// Deny records a denial of ns/name; an approval in force is revoked (its
// entry removed when the approval added it and no other approval needs
// it).
func (s *Service) Deny(ctx context.Context, ns, name, note, actor string) (*v1alpha1.UnblockRequest, error) {
	r, err := s.load(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.end(ctx, r, now); err != nil {
		return nil, err
	}
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	st := v1alpha1.UnblockRequestStatus{State: v1alpha1.UnblockDenied, DecidedBy: actor, DecidedAt: &at, Note: note, History: pushHistory(r.Status)}
	return s.setStatus(ctx, r, st)
}

// Delete deletes ns/name. A time-limited approval in force answers
// ErrState: its entry would never be removed.
func (s *Service) Delete(ctx context.Context, ns, name string) (*v1alpha1.UnblockRequest, error) {
	r, err := s.load(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	if Active(r, s.now()) && r.Status.Allow.ExpiresAt != nil {
		return nil, fmt.Errorf("%w: %s/%s is a time-limited approval in force; deny it first (which removes its allow entry)", ErrState, ns, name)
	}
	if err := s.opts.Store.Delete(ctx, keyOf(ns, name), store.DeleteOptions{Manager: Manager, Force: true}); err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.items, keyOf(ns, name))
	s.mu.Unlock()
	return r, nil
}

// setStatus writes st onto r and returns the stored request.
func (s *Service) setStatus(ctx context.Context, r *v1alpha1.UnblockRequest, st v1alpha1.UnblockRequestStatus) (*v1alpha1.UnblockRequest, error) {
	next := r.DeepCopy()
	next.Status = st
	if err := s.opts.Store.UpdateStatus(ctx, next); err != nil {
		return nil, err
	}
	if fresh := s.reread(ctx, keyOf(r.Namespace, r.Name)); fresh != nil {
		next = fresh
	}
	s.upsert(next)
	return next, nil
}

// policiesOfGroup names the policies in ns whose clientGroupRefs include
// group.
func (s *Service) policiesOfGroup(ctx context.Context, ns, group string) ([]string, error) {
	var out []string
	err := s.each(ctx, "Policy", ns, func(o store.Object) {
		if p, ok := o.(*v1alpha1.Policy); ok && slices.Contains(p.Spec.ClientGroupRefs, group) {
			out = append(out, p.Name)
		}
	})
	slices.Sort(out)
	return out, err
}

// each lists every object of kind in ns.
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

// writeAttempts bounds the read-modify-write retries of list and policy
// edits that lose a race with another writer.
const writeAttempts = 3

// writable reports whether the API may change obj and whether it must
// force (bootstrap defaults), as the API's own write path decides.
func writable(obj store.Object) (ok, force bool) {
	switch store.ManagedBy(obj) {
	case "", store.ManagerAPI:
		return true, false
	case store.ManagerBootstrap:
		return true, true
	}
	return false, false
}

// addEntry adds entry to the "ui" source of the allow list ns/name,
// creating the list. added is false when the entry was there already.
func (s *Service) addEntry(ctx context.Context, ns, name, entry string) (added bool, err error) {
	for attempt := 1; ; attempt++ {
		added, err = s.tryAddEntry(ctx, ns, name, entry)
		if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
			return added, err
		}
	}
}

func (s *Service) tryAddEntry(ctx context.Context, ns, name, entry string) (bool, error) {
	bl := &v1alpha1.BlockList{}
	opts := store.ApplyOptions{Manager: store.ManagerAPI}
	err := s.opts.Store.Get(ctx, store.Key{Kind: "BlockList", Namespace: ns, Name: name}, bl)
	switch {
	case errors.Is(err, store.ErrNotFound):
		bl = &v1alpha1.BlockList{
			TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "BlockList"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{{Name: uiSource, Inline: []string{entry}}}},
		}
		_, err := s.opts.Store.Apply(ctx, bl, opts)
		return true, err
	case err != nil:
		return false, err
	}
	ok, force := writable(bl)
	if !ok {
		return false, &store.ErrManagedBy{Key: store.KeyOf(bl), Owner: store.ManagedBy(bl)}
	}
	if bl.Spec.Mode != "allow" {
		return false, fmt.Errorf("%w: BlockList %s/%s has mode %q, not allow", ErrInvalid, ns, name, bl.Spec.Mode)
	}
	idx := slices.IndexFunc(bl.Spec.Sources, func(src v1alpha1.ListSource) bool { return src.Name == uiSource })
	if idx < 0 {
		idx = slices.IndexFunc(bl.Spec.Sources, func(src v1alpha1.ListSource) bool { return src.URL == "" && src.ConfigMapRef == nil })
	}
	if idx < 0 {
		bl.Spec.Sources = append(bl.Spec.Sources, v1alpha1.ListSource{Name: uiSource})
		idx = len(bl.Spec.Sources) - 1
	}
	if slices.Contains(bl.Spec.Sources[idx].Inline, entry) {
		return false, nil
	}
	bl.Spec.Sources[idx].Inline = append(bl.Spec.Sources[idx].Inline, entry)
	opts.ExpectedResourceVersion, opts.Force = bl.ResourceVersion, force
	_, err = s.opts.Store.Apply(ctx, bl, opts)
	return true, err
}

// removeEntry takes entry off the allow list ns/name. A source left empty
// is dropped; a list left without sources is deleted and detached from
// policies.
func (s *Service) removeEntry(ctx context.Context, ns, name, entry string) error {
	for attempt := 1; ; attempt++ {
		err := s.tryRemoveEntry(ctx, ns, name, entry)
		if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
			return err
		}
	}
}

func (s *Service) tryRemoveEntry(ctx context.Context, ns, name, entry string) error {
	bl := &v1alpha1.BlockList{}
	key := store.Key{Kind: "BlockList", Namespace: ns, Name: name}
	if err := s.opts.Store.Get(ctx, key, bl); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	ok, force := writable(bl)
	if !ok {
		return &store.ErrManagedBy{Key: key, Owner: store.ManagedBy(bl)}
	}
	changed := false
	sources := bl.Spec.Sources[:0:0]
	for _, src := range bl.Spec.Sources {
		if src.URL == "" && src.ConfigMapRef == nil && slices.Contains(src.Inline, entry) {
			src.Inline = slices.DeleteFunc(slices.Clone(src.Inline), func(e string) bool { return e == entry })
			changed = true
			if len(src.Inline) == 0 {
				continue
			}
		}
		sources = append(sources, src)
	}
	if !changed {
		return nil
	}
	if len(sources) == 0 {
		if err := s.opts.Store.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI, Force: force}); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return s.detachEverywhere(ctx, ns, name)
	}
	bl.Spec.Sources = sources
	_, err := s.opts.Store.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: bl.ResourceVersion, Force: force})
	return err
}

// attach adds list to the allowListRefs of policy ns/name. ok is false
// when the policy is managed elsewhere and was left alone.
func (s *Service) attach(ctx context.Context, ns, name, list string) (ok bool, err error) {
	for attempt := 1; ; attempt++ {
		ok, err = s.editPolicy(ctx, ns, name, func(p *v1alpha1.Policy) bool {
			if slices.Contains(p.Spec.AllowListRefs, list) {
				return false
			}
			p.Spec.AllowListRefs = append(p.Spec.AllowListRefs, list)
			return true
		})
		if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
			return ok, err
		}
	}
}

// detachEverywhere removes list from the allowListRefs of every writable
// policy in ns.
func (s *Service) detachEverywhere(ctx context.Context, ns, list string) error {
	var names []string
	if err := s.each(ctx, "Policy", ns, func(o store.Object) {
		if p, ok := o.(*v1alpha1.Policy); ok && slices.Contains(p.Spec.AllowListRefs, list) {
			names = append(names, p.Name)
		}
	}); err != nil {
		return err
	}
	for _, n := range names {
		for attempt := 1; ; attempt++ {
			_, err := s.editPolicy(ctx, ns, n, func(p *v1alpha1.Policy) bool {
				before := len(p.Spec.AllowListRefs)
				p.Spec.AllowListRefs = slices.DeleteFunc(p.Spec.AllowListRefs, func(r string) bool { return r == list })
				return len(p.Spec.AllowListRefs) != before
			})
			if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				break
			}
		}
	}
	return nil
}

// editPolicy applies fn to policy ns/name and writes it when fn changed
// it. ok is false when the policy is not writable by the API.
func (s *Service) editPolicy(ctx context.Context, ns, name string, fn func(*v1alpha1.Policy) bool) (bool, error) {
	p := &v1alpha1.Policy{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "Policy", Namespace: ns, Name: name}, p); err != nil {
		return false, err
	}
	ok, force := writable(p)
	if !ok {
		return false, nil
	}
	if !fn(p) {
		return true, nil
	}
	_, err := s.opts.Store.Apply(ctx, p, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: p.ResourceVersion, Force: force})
	return true, err
}

// end releases the allow entry of an approved r and marks it removed in
// r (in memory; the caller writes the status).
func (s *Service) end(ctx context.Context, r *v1alpha1.UnblockRequest, now time.Time) error {
	if r.EffectiveState() != v1alpha1.UnblockApproved || r.Status.Allow == nil || r.Status.Allow.Removed != nil {
		return nil
	}
	if err := s.release(ctx, r, now); err != nil {
		return err
	}
	t := metav1.NewTime(now.UTC().Truncate(time.Second))
	r.Status.Allow.Removed = &t
	return nil
}

// release ends r's allow entry: it is removed when r added it and no
// other approval in force needs it; otherwise such an approval inherits
// the removal (status.allow.added). r's status is not written.
func (s *Service) release(ctx context.Context, r *v1alpha1.UnblockRequest, now time.Time) error {
	a := r.Status.Allow
	if a == nil || a.Removed != nil {
		return nil
	}
	var heir *v1alpha1.UnblockRequest
	if err := s.each(ctx, Kind, "", func(o store.Object) {
		other, ok := o.(*v1alpha1.UnblockRequest)
		if !ok || heir != nil || (other.Namespace == r.Namespace && other.Name == r.Name) || !Active(other, now) {
			return
		}
		if oa := other.Status.Allow; oa.List == a.List && oa.Entry == a.Entry {
			heir = other
		}
	}); err != nil {
		return err
	}
	if heir != nil {
		if a.Added && !heir.Status.Allow.Added {
			st := *heir.Status.DeepCopy()
			st.Allow.Added = true
			if _, err := s.setStatus(ctx, heir, st); err != nil {
				return err
			}
		}
		return nil
	}
	if !a.Added {
		return nil
	}
	ns, name, _ := strings.Cut(a.List, "/")
	return s.removeEntry(ctx, ns, name, a.Entry)
}
