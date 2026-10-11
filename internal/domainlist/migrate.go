package domainlist

import (
	"context"
	"errors"
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

// RunMigration runs Migrate until it succeeds (leader only), then waits
// for ctx to end.
func (s *Service) RunMigration(ctx context.Context) error {
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
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
	<-ctx.Done()
	return nil
}
