package domainlist

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Migrate converts the legacy inline lines of every ui list kudurru may
// edit (written by the Domains page, the query log and unblock approvals
// before structured entries) into entries, once. A line an approval in
// status.allow still holds becomes an entry with origin unblock and the
// request. Lists managed elsewhere are left alone; they are read in both
// forms. It returns how many lists it rewrote.
func (s *Service) Migrate(ctx context.Context) (int, error) {
	prov := provenance{}
	if err := s.each(ctx, store.Kind(v1alpha1.KindUnblockRequest), "", func(o store.Object) {
		r, ok := o.(*v1alpha1.UnblockRequest)
		if !ok || r.Status.Allow == nil || r.Status.Allow.Removed != nil {
			return
		}
		prov[r.Status.Allow.List+"\x00"+RuleKey(r.Status.Allow.Entry)] = r.Namespace + "/" + r.Name
	}); err != nil {
		return 0, err
	}
	var todo []store.Key
	if err := s.each(ctx, "BlockList", "", func(o store.Object) {
		bl, ok := o.(*v1alpha1.BlockList)
		if !ok || !writableOwner(store.ManagedBy(bl)) {
			return
		}
		if a, _, ok := ParseListName(bl.Name); ok && listMode(bl) == a.Mode() && migrateList(bl.DeepCopy(), nil) {
			todo = append(todo, store.KeyOf(bl))
		}
	}); err != nil {
		return 0, err
	}
	n := 0
	for _, key := range todo {
		for attempt := 1; ; attempt++ {
			ok, err := s.migrateOne(ctx, key, prov)
			if errors.Is(err, store.ErrConflict) && attempt < writeAttempts {
				continue
			}
			if err != nil {
				return n, err
			}
			if ok {
				n++
			}
			break
		}
	}
	return n, nil
}

func (s *Service) migrateOne(ctx context.Context, key store.Key, prov provenance) (bool, error) {
	bl := &v1alpha1.BlockList{}
	if err := s.opts.Store.Get(ctx, key, bl); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	owner := store.ManagedBy(bl)
	if !writableOwner(owner) || !migrateList(bl, prov) {
		return false, nil
	}
	_, err := s.opts.Store.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: bl.ResourceVersion, Force: owner == store.ManagerBootstrap})
	return err == nil, err
}

// ReconcileInterval is how often the leader checks the policy references
// of the ui lists.
const ReconcileInterval = 30 * time.Second

// Reconcile keeps the policy references of the ui lists of every namespace
// as the scopes promise: an everyone list is referenced from every policy
// kudurru may edit (also one created after the entries), a group list
// from exactly the writable policies whose clientGroupRefs name the group
// (a policy that stopped naming the group loses it). Policies managed
// elsewhere are left alone. It returns how many references it changed.
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	var lists []*v1alpha1.BlockList
	if err := s.each(ctx, "BlockList", "", func(o store.Object) {
		if bl, ok := o.(*v1alpha1.BlockList); ok {
			if a, _, ok := ParseListName(bl.Name); ok && listMode(bl) == a.Mode() {
				lists = append(lists, bl)
			}
		}
	}); err != nil || len(lists) == 0 {
		return 0, err
	}
	pols := map[string][]*v1alpha1.Policy{}
	if err := s.each(ctx, "Policy", "", func(o store.Object) {
		if p, ok := o.(*v1alpha1.Policy); ok && writableOwner(store.ManagedBy(p)) {
			pols[p.Namespace] = append(pols[p.Namespace], p)
		}
	}); err != nil {
		return 0, err
	}
	n := 0
	for _, bl := range lists {
		a, group, _ := ParseListName(bl.Name)
		for _, p := range pols[bl.Namespace] {
			want := group == "" || slices.Contains(p.Spec.ClientGroupRefs, group)
			if want == slices.Contains(refs(a, p), bl.Name) {
				continue
			}
			changed, err := s.setRef(ctx, a, p.Namespace, p.Name, bl.Name, want)
			if err != nil {
				return n, err
			}
			if changed {
				n++
			}
		}
	}
	return n, nil
}

// setRef adds (on) or removes list from the references of action a of
// policy ns/name, retrying conflicts.
func (s *Service) setRef(ctx context.Context, a Action, ns, name, list string, on bool) (bool, error) {
	for attempt := 1; ; attempt++ {
		changed := false
		_, err := s.editPolicy(ctx, ns, name, func(p *v1alpha1.Policy) bool {
			r := &p.Spec.BlockListRefs
			if a == Allow {
				r = &p.Spec.AllowListRefs
			}
			has := slices.Contains(*r, list)
			switch {
			case on && !has:
				*r = append(*r, list)
			case !on && has:
				*r = slices.DeleteFunc(*r, func(x string) bool { return x == list })
			default:
				return false
			}
			changed = true
			return true
		})
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
			return changed && err == nil, err
		}
	}
}

// Run is the leader's runnable: Migrate until it succeeds, then Reconcile
// every ReconcileInterval until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	for delay := time.Second; ; delay = min(delay*2, time.Minute) {
		n, err := s.Migrate(ctx)
		if err == nil {
			if n > 0 {
				s.opts.Logger.Info("allow and deny lists migrated to structured entries", "lists", n)
			}
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		s.opts.Logger.Warn("allow and deny list migration failed; retrying", "err", err, "retryIn", delay)
		if !sleep(ctx, delay) {
			return nil
		}
	}
	for {
		if n, err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.opts.Logger.Warn("allow and deny list references", "err", err)
		} else if n > 0 {
			s.opts.Logger.Info("allow and deny lists referenced from their policies", "changes", n)
		}
		if !sleep(ctx, ReconcileInterval) {
			return nil
		}
	}
}

// sleep waits d; false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
