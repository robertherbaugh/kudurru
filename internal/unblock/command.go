package unblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Action is the closed set of public request commands. Expiry is a separate
// system operation; it never borrows an HTTP credential or decision grant.
type Action string

const (
	Approve Action = "approve"
	Deny    Action = "deny"
	Delete  Action = "delete"
)

// Target binds the request object whose current UID/version determines effects.
type Target struct {
	Namespace, Name string
	Action          Action
}

func (t Target) Valid() bool {
	return t.Namespace != "" && len(t.Namespace) <= 63 && t.Name != "" && len(t.Name) <= 253 && (t.Action == Approve || t.Action == Deny || t.Action == Delete)
}

// Command must authenticate the original proof for the entire closed action
// before ExecuteCurrent and retain writer/dependency exclusion through commit.
type Command func(context.Context, Target, Decision) (*Outcome, error)

const maxCommandObjects = 4096
const maxCommandBytes = 32 << 20

type currentEdit struct {
	old, desired store.Object
	status       bool
}
type currentPlan struct {
	tx             *sqlite.CommandTx
	edits          map[store.Key]*currentEdit
	order          []store.Key
	objects, bytes int
	requestRows    []*public.UnblockRequest
	requestsLoaded bool
}

func newCurrentPlan(tx *sqlite.CommandTx) *currentPlan {
	return &currentPlan{tx: tx, edits: map[store.Key]*currentEdit{}}
}
func writable(obj store.Object) bool {
	switch store.ManagedBy(obj) {
	case "", store.ManagerAPI, store.ManagerBootstrap:
		return true
	}
	return false
}
func (p *currentPlan) budget(obj store.Object) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	p.objects++
	p.bytes += len(raw)
	if p.objects > maxCommandObjects || len(raw) > 1<<20 || p.bytes > maxCommandBytes {
		return store.ErrTooLarge
	}
	return nil
}
func (p *currentPlan) each(ctx context.Context, kind store.Kind, ns string, fn func(store.Object) error) error {
	cont := ""
	for {
		page, err := p.tx.List(ctx, kind, store.ListOptions{Namespace: ns, Limit: 500, Continue: cont})
		if err != nil {
			return err
		}
		for _, obj := range page.Items {
			if err := p.budget(obj); err != nil {
				return err
			}
			if e, ok := p.edits[store.KeyOf(obj)]; ok {
				obj = e.desired
				if obj == nil {
					continue
				}
			}
			if err := fn(obj); err != nil {
				return err
			}
		}
		if page.Continue == "" {
			return nil
		}
		cont = page.Continue
	}
}
func (p *currentPlan) edit(old, desired store.Object, status bool) error {
	if old != nil && reflect.ValueOf(old).IsNil() {
		old = nil
	}
	if desired != nil && reflect.ValueOf(desired).IsNil() {
		desired = nil
	}
	var key store.Key
	if old == nil {
		key = store.KeyOf(desired)
	} else {
		key = store.KeyOf(old)
	}
	if desired != nil {
		if err := p.budget(desired); err != nil {
			return err
		}
	}
	if e, ok := p.edits[key]; ok {
		e.desired = desired
		return nil
	}
	p.edits[key] = &currentEdit{old: old, desired: desired, status: status}
	p.order = append(p.order, key)
	return nil
}
func (p *currentPlan) list(ctx context.Context, ns, name string) (*public.BlockList, error) {
	key := store.Key{Kind: "BlockList", Namespace: ns, Name: name}
	if e, ok := p.edits[key]; ok {
		if e.desired == nil {
			return nil, nil
		}
		return e.desired.(*public.BlockList), nil
	}
	var bl public.BlockList
	if err := p.tx.Get(ctx, key, &bl); errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := p.budget(&bl); err != nil {
		return nil, err
	}
	return &bl, nil
}
func (p *currentPlan) policies(ctx context.Context, ns, list, group string, attach bool) (attached, skipped []string, err error) {
	err = p.each(ctx, "Policy", ns, func(obj store.Object) error {
		pol := obj.(*public.Policy)
		if attach && group != "" && !slices.Contains(pol.Spec.ClientGroupRefs, group) {
			return nil
		}
		if !attach && !slices.Contains(pol.Spec.AllowListRefs, list) && !slices.Contains(pol.Spec.BlockListRefs, list) {
			return nil
		}
		if !writable(pol) || pol.DeletionTimestamp != nil {
			skipped = append(skipped, ns+"/"+pol.Name)
			return nil
		}
		attached = append(attached, ns+"/"+pol.Name)
		desired := pol.DeepCopy()
		if attach {
			if !slices.Contains(desired.Spec.AllowListRefs, list) {
				desired.Spec.AllowListRefs = append(desired.Spec.AllowListRefs, list)
			}
		} else {
			desired.Spec.AllowListRefs = slices.DeleteFunc(desired.Spec.AllowListRefs, func(s string) bool { return s == list })
			desired.Spec.BlockListRefs = slices.DeleteFunc(desired.Spec.BlockListRefs, func(s string) bool { return s == list })
		}
		if reflect.DeepEqual(pol.Spec, desired.Spec) {
			return nil
		}
		desired.TypeMeta = metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "Policy"}
		return p.edit(pol, desired, false)
	})
	return
}
func approvalEditError(err error) error {
	if errors.Is(err, domainlist.ErrInvalid) {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return err
}

func derivedEntry(r *public.UnblockRequest, a *public.UnblockAllow) (store.Key, public.ListEntry, error) {
	ns, list := r.Namespace, EveryoneList
	if a.Scope == ScopeGroup {
		var group string
		var ok bool
		ns, group, ok = strings.Cut(a.Group, "/")
		if !ok || ns == "" || group == "" {
			return store.Key{}, public.ListEntry{}, fmt.Errorf("%w: invalid approval group", ErrInvalid)
		}
		list = domainlist.ListName(domainlist.Allow, group)
	} else if a.Scope != ScopeEveryone {
		return store.Key{}, public.ListEntry{}, fmt.Errorf("%w: invalid approval scope", ErrInvalid)
	}
	if a.Match != MatchExact && a.Match != MatchSubdomains {
		return store.Key{}, public.ListEntry{}, fmt.Errorf("%w: invalid approval match", ErrInvalid)
	}
	typ, value := public.EntryExact, r.Spec.Domain
	if a.Match == MatchSubdomains {
		typ, value = public.EntryWildcard, "*."+value
	}
	normalized, err := domainlist.Normalize(typ, value)
	if err != nil {
		return store.Key{}, public.ListEntry{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if a.List != ns+"/"+list || domainlist.RuleKey(a.Entry) != domainlist.RuleKey(Entry(r.Spec.Domain, a.Match)) {
		return store.Key{}, public.ListEntry{}, fmt.Errorf("%w: approval effects do not match request", ErrInvalid)
	}
	return store.Key{Kind: "BlockList", Namespace: ns, Name: list}, public.ListEntry{Type: typ, Value: normalized, Origin: public.OriginUnblock, Request: r.Namespace + "/" + r.Name}, nil
}

// requests reads the durable request set once per plan. Shared-ownership
// checks reuse that bounded cut and overlay earlier planned status changes.
func (p *currentPlan) requests(ctx context.Context) ([]*public.UnblockRequest, error) {
	if !p.requestsLoaded {
		if err := p.each(ctx, Kind, "", func(obj store.Object) error {
			p.requestRows = append(p.requestRows, obj.(*public.UnblockRequest))
			return nil
		}); err != nil {
			return nil, err
		}
		p.requestsLoaded = true
	}
	return p.requestRows, nil
}
func (p *currentPlan) release(ctx context.Context, r *public.UnblockRequest, now time.Time) error {
	a := r.Status.Allow
	if r.EffectiveState() != public.UnblockApproved || a == nil || a.Removed != nil {
		return nil
	}
	key, entry, err := derivedEntry(r, a)
	if err != nil {
		return err
	}
	var heir *public.UnblockRequest
	rows, err := p.requests(ctx)
	if err != nil {
		return err
	}
	for _, other := range rows {
		if e, ok := p.edits[store.KeyOf(other)]; ok {
			if e.desired == nil {
				continue
			}
			other = e.desired.(*public.UnblockRequest)
		}
		if store.KeyOf(other) == store.KeyOf(r) || !Active(other, now) {
			continue
		}
		oa := other.Status.Allow
		if oa.List != a.List || domainlist.RuleKey(oa.Entry) != domainlist.RuleKey(a.Entry) {
			continue
		}
		if _, _, err := derivedEntry(other, oa); err != nil {
			return err
		}
		if store.ManagedBy(other) != Manager || other.DeletionTimestamp != nil {
			return &store.ErrManagedBy{Key: store.KeyOf(other), Owner: store.ManagedBy(other)}
		}
		heir = other
		break
	}

	if a.Added {
		if heir != nil {
			if !heir.Status.Allow.Added {
				desired := heir.DeepCopy()
				desired.Status.Allow.Added = true
				if err := p.edit(heir, desired, true); err != nil {
					return err
				}
			}
		} else {
			old, err := p.list(ctx, key.Namespace, key.Name)
			if err != nil {
				return err
			}
			edit, err := domainlist.PlanApprovalEntry(old, key, entry, true)
			if err != nil {
				return approvalEditError(err)
			}
			if edit.Changed {
				var desired store.Object
				if edit.Desired != nil {
					desired = edit.Desired
				}
				if err := p.edit(old, desired, false); err != nil {
					return err
				}
				if edit.Desired == nil {
					if _, _, err := p.policies(ctx, key.Namespace, key.Name, "", false); err != nil {
						return err
					}
				}
			}
		}
	}
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	a.Removed = &at
	return nil
}
func (p *currentPlan) apply(ctx context.Context) error {
	for _, key := range p.order {
		e := p.edits[key]
		if e.old != nil {
			// Even status writes check the original request identity and manager.
			if e.old.GetDeletionTimestamp() != nil {
				return store.ErrConflict
			}
			if e.status || key.Kind == Kind {
				if store.ManagedBy(e.old) != Manager {
					return &store.ErrManagedBy{Key: key, Owner: store.ManagedBy(e.old)}
				}
			} else if !writable(e.old) {
				return &store.ErrManagedBy{Key: key, Owner: store.ManagedBy(e.old)}
			}
			current := e.old.DeepCopyObject().(store.Object)
			if err := p.tx.Get(ctx, key, current); err != nil {
				return err
			}
			if current.GetUID() != e.old.GetUID() || current.GetResourceVersion() != e.old.GetResourceVersion() {
				return store.ErrConflict
			}
		}
	}
	for _, key := range p.order {
		e := p.edits[key]
		switch {
		case e.desired == nil:
			manager := store.ManagerAPI
			if key.Kind == Kind {
				manager = Manager
			}
			if err := p.tx.Delete(ctx, key, store.DeleteOptions{Manager: manager, Force: store.ManagedBy(e.old) == store.ManagerBootstrap}); err != nil {
				return err
			}
		case e.status:
			if err := p.tx.UpdateStatus(ctx, e.desired); err != nil {
				return err
			}
		default:
			opt := store.ApplyOptions{Manager: store.ManagerAPI}
			if e.old != nil {
				opt.ExpectedResourceVersion = e.old.GetResourceVersion()
				opt.Force = store.ManagedBy(e.old) == store.ManagerBootstrap
			}
			if _, err := p.tx.Apply(ctx, e.desired, opt); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExecuteCurrent plans only bounded effects derived from a current request and
// stages them in tx. Caller must already have authenticated its original raw
// credential for Target.Action, including no-op/error paths. No live Store,
// retries, index writes, cache effects or network operations occur here.
func ExecuteCurrent(ctx context.Context, tx *sqlite.CommandTx, target Target, d Decision, now time.Time) (*Outcome, error) {
	if tx == nil || !target.Valid() {
		return nil, fmt.Errorf("%w: invalid current request command", ErrInvalid)
	}
	p := newCurrentPlan(tx)
	var current public.UnblockRequest
	if err := tx.Get(ctx, keyOf(target.Namespace, target.Name), &current); err != nil {
		return nil, err
	}
	if store.ManagedBy(&current) != Manager {
		return nil, &store.ErrManagedBy{Key: store.KeyOf(&current), Owner: store.ManagedBy(&current)}
	}
	if current.DeletionTimestamp != nil {
		return nil, store.ErrConflict
	}
	if len(d.Note) > 500 {
		return nil, fmt.Errorf("%w: note too long", ErrInvalid)
	}
	r := current.DeepCopy()
	out := &Outcome{Request: r}
	switch target.Action {
	case Delete:
		if Active(r, now) && r.Status.Allow.ExpiresAt != nil {
			return nil, fmt.Errorf("%w: time-limited approval in force; deny it first", ErrState)
		}
		if r.Status.Allow != nil && r.Status.Allow.ExpiresAt != nil && !r.Status.Allow.ExpiresAt.After(now) {
			if err := p.release(ctx, r, now); err != nil {
				return nil, err
			}
		}
		if err := p.edit(&current, nil, false); err != nil {
			return nil, err
		}
	case Deny:
		if err := p.release(ctx, r, now); err != nil {
			return nil, err
		}
		at := metav1.NewTime(now.UTC().Truncate(time.Second))
		r.Status = public.UnblockRequestStatus{State: public.UnblockDenied, DecidedBy: d.Actor, DecidedAt: &at, Note: d.Note, History: pushHistory(r.Status)}
		if err := p.edit(&current, r, true); err != nil {
			return nil, err
		}
	case Approve:
		if Active(r, now) {
			return nil, fmt.Errorf("%w: already approved; deny it to revoke the approval", ErrState)
		}
		if d.Scope == "" {
			d.Scope = ScopeEveryone
		}
		if d.Match == "" {
			d.Match = MatchExact
		}
		if d.Duration < 0 || d.Duration > 366*24*time.Hour || len(d.Note) > 500 {
			return nil, fmt.Errorf("%w: invalid duration or note", ErrInvalid)
		}
		ns, list, group := r.Namespace, EveryoneList, ""
		switch d.Scope {
		case ScopeEveryone:
		case ScopeGroup:
			group = d.Group
			if group == "" && len(r.Spec.Groups) > 0 {
				group = r.Spec.Groups[0]
			}
			if group == "" {
				return nil, fmt.Errorf("%w: the request names no client group", ErrInvalid)
			}
			if !strings.Contains(group, "/") {
				group = ns + "/" + group
			}
			var name string
			ns, name, _ = strings.Cut(group, "/")
			list = domainlist.ListName(domainlist.Allow, name)
			var cg public.ClientGroup
			if err := tx.Get(ctx, store.Key{Kind: "ClientGroup", Namespace: ns, Name: name}, &cg); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("%w: no client group %s", ErrInvalid, group)
				}
				return nil, err
			}
			if cg.DeletionTimestamp != nil {
				return nil, store.ErrConflict
			}
		default:
			return nil, fmt.Errorf("%w: invalid scope", ErrInvalid)
		}
		a := &public.UnblockAllow{Scope: d.Scope, Match: d.Match, Group: group, List: ns + "/" + list, Entry: Entry(r.Spec.Domain, d.Match)}
		key, entry, err := derivedEntry(r, a)
		if err != nil {
			return nil, err
		}
		if err := p.release(ctx, r, now); err != nil {
			return nil, err
		}
		old, err := p.list(ctx, ns, list)
		if err != nil {
			return nil, err
		}
		at := metav1.NewTime(now.UTC().Truncate(time.Second))
		entry.Created = &at
		entry.CreatedBy = d.Actor
		entry.Comment = d.Note
		edit, err := domainlist.PlanApprovalEntry(old, key, entry, false)
		if err != nil {
			return nil, approvalEditError(err)
		}
		if edit.Changed {
			if err := p.edit(old, edit.Desired, false); err != nil {
				return nil, err
			}
		}
		_, gname, _ := strings.Cut(group, "/")
		out.Attached, out.Skipped, err = p.policies(ctx, ns, list, gname, true)
		if err != nil {
			return nil, err
		}
		if group != "" && len(out.Attached)+len(out.Skipped) == 0 {
			return nil, fmt.Errorf("%w: no policy covers client group %s", ErrInvalid, group)
		}
		if len(out.Attached) > 64 {
			return nil, store.ErrTooLarge
		}
		a.Added, a.Policies = edit.Added, out.Attached
		if d.Duration > 0 {
			exp := metav1.NewTime(now.Add(d.Duration).UTC().Truncate(time.Second))
			a.ExpiresAt = &exp
		}
		r.Status = public.UnblockRequestStatus{State: public.UnblockApproved, DecidedBy: d.Actor, DecidedAt: &at, Note: d.Note, Allow: a, History: pushHistory(r.Status)}
		if err := p.edit(&current, r, true); err != nil {
			return nil, err
		}
	}
	if err := p.apply(ctx); err != nil {
		return nil, err
	}
	if target.Action != Delete {
		if err := tx.Get(ctx, store.KeyOf(r), r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SweepCurrent atomically ends approvals, expires pending requests and prunes
// retention history. Its caller is the original elected system writer; no user
// permission is borrowed for background cleanup. It works from durable rows on
// restart, independently of the request watch index.
func SweepCurrent(ctx context.Context, tx *sqlite.CommandTx, now time.Time, pendingExpiry, retention time.Duration) (SweepResult, error) {
	var res SweepResult
	if tx == nil || pendingExpiry <= 0 || retention <= 0 {
		return res, fmt.Errorf("%w: invalid current sweep", ErrInvalid)
	}
	p := newCurrentPlan(tx)
	all, err := p.requests(ctx)
	if err != nil {
		return res, err
	}
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	for _, old := range all {
		if e, ok := p.edits[store.KeyOf(old)]; ok && e.desired != nil {
			old = e.desired.(*public.UnblockRequest)
		}
		r := old.DeepCopy()
		switch {
		case r.EffectiveState() == public.UnblockApproved && r.Status.Allow != nil && r.Status.Allow.Removed == nil && r.Status.Allow.ExpiresAt != nil && !r.Status.Allow.ExpiresAt.After(now):
			if err := p.release(ctx, r, now); err != nil {
				return SweepResult{}, err
			}
			r.Status.State = public.UnblockExpired
			if err := p.edit(old, r, true); err != nil {
				return SweepResult{}, err
			}
			res.Ended++
		case r.EffectiveState() == public.UnblockPending && now.Sub(RequestedAt(r)) >= pendingExpiry:
			r.Status.State, r.Status.DecidedBy, r.Status.DecidedAt = public.UnblockExpired, SystemActor, &at
			r.Status.Note = "No administrator decided within " + pendingExpiry.String() + "."
			if err := p.edit(old, r, true); err != nil {
				return SweepResult{}, err
			}
			res.Expired++
		case r.EffectiveState() != public.UnblockPending && (!Active(r, now) || r.Status.Allow.ExpiresAt == nil):
			if decided := decidedAt(r); !decided.IsZero() && now.Sub(decided) >= retention {
				if err := p.edit(old, nil, false); err != nil {
					return SweepResult{}, err
				}
				res.Pruned++
			}
		}
	}
	if err := p.apply(ctx); err != nil {
		return SweepResult{}, err
	}
	return res, nil
}
