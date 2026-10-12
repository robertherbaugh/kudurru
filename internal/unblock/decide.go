package unblock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Allow lists: approvals for everyone go to the Allowlist's everyone list
// (ui-allow), approvals for one ClientGroup to ui-allow-<group> in the
// group's namespace, as structured entries (internal/domainlist) with
// origin unblock and the request.
const (
	EveryoneList    = domainlist.AllowList
	GroupListPrefix = domainlist.AllowList + "-"
	// DefaultPolicy is the bootstrap policy.
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

// Entry is the allow-list rule of domain for match: the name, or
// "*.name" for the name and its subdomains.
func Entry(domain, match string) string {
	if match == MatchSubdomains {
		return v1alpha1.EntryRule(v1alpha1.EntryWildcard, "*."+domain)
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
	stored := *r.Status.DeepCopy()
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
	var scope domainlist.Scope
	switch d.Scope {
	case ScopeEveryone, "":
		d.Scope = ScopeEveryone
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
		listNS, list = gns, domainlist.ListName(domainlist.Allow, gname)
		scope.Groups = []string{gname}
	default:
		return nil, fmt.Errorf("%w: scope %q (everyone or group)", ErrInvalid, d.Scope)
	}
	// An ended time-limited approval the janitor has not processed yet
	// gives up its entry first, so the new one owns it.
	if err := s.end(ctx, r, now); err != nil {
		return nil, err
	}
	// From here on the earlier approval's entry may be gone: a failure
	// records that (status.allow.removed) instead of leaving the request
	// approved with nothing behind it.
	ended := stored.Allow != nil && stored.Allow.Removed == nil && r.Status.Allow != nil && r.Status.Allow.Removed != nil
	fail := func(err error) (*Outcome, error) {
		if ended {
			if _, werr := s.setStatus(ctx, r, r.Status); werr != nil {
				err = errors.Join(err, werr)
			}
		}
		return nil, err
	}
	typ, value := v1alpha1.EntryExact, r.Spec.Domain
	if d.Match == MatchSubdomains {
		typ, value = v1alpha1.EntryWildcard, "*."+r.Spec.Domain
	}
	res, err := s.lists.Add(ctx, domainlist.AddRequest{
		Action: domainlist.Allow, Namespace: listNS, Type: typ, Values: []string{value}, Scope: scope,
		Comment: d.Note, Origin: v1alpha1.OriginUnblock, Request: ns + "/" + name, Actor: d.Actor, KeepExisting: true,
	})
	switch {
	case errors.Is(err, domainlist.ErrNoPolicy):
		return fail(fmt.Errorf("%w: no policy covers client group %s; approve it for everyone", ErrInvalid, group))
	case errors.Is(err, domainlist.ErrInvalid):
		return fail(fmt.Errorf("%w: %v", ErrInvalid, err))
	case err != nil:
		return fail(err)
	case len(res.Errors) > 0:
		return fail(fmt.Errorf("%w: %v", ErrInvalid, res.Errors[0].Err))
	}
	entry := Entry(r.Spec.Domain, d.Match)
	added := len(res.New) > 0
	out := &Outcome{Attached: res.Attached, Skipped: res.Skipped}
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
	out.Request, err = s.setStatus(ctx, r, st)
	if errors.Is(err, store.ErrConflict) {
		// The block page updated the request (its spec) since it was
		// read: record the decision on the fresh object, unless someone
		// decided it meanwhile.
		if fresh, ferr := s.load(ctx, ns, name); ferr == nil && equality.Semantic.DeepEqual(fresh.Status, stored) {
			out.Request, err = s.setStatus(ctx, fresh, st)
		}
	}
	if err != nil {
		if added {
			// No request links to the entry now: a retry would find it
			// present, not own it, and never remove it.
			if rerr := s.lists.RemoveRule(ctx, listNS, list, entry, v1alpha1.OriginUnblock); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
		return fail(err)
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
		if oa := other.Status.Allow; oa.List == a.List && domainlist.RuleKey(oa.Entry) == domainlist.RuleKey(a.Entry) {
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
	return s.lists.RemoveRule(ctx, ns, name, a.Entry, v1alpha1.OriginUnblock)
}
