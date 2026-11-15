package main

import (
	"context"
	"errors"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// sqliteObjectSources is installed before the first loader.Load. The loader's
// immutable accepted cut is a host input receipt, not a credential or writer
// token. Startup initialization remains exclusively in openStore.
type sqliteObjectSources struct {
	store *sqlite.Store
	now   func() time.Time
}

func newSQLiteObjectSources(st *sqlite.Store) *sqliteObjectSources {
	return &sqliteObjectSources{store: st, now: time.Now}
}

func (s *sqliteObjectSources) ApplyRoleBinding(ctx context.Context, plan objects.RoleBindingPlan) (*public.RoleBinding, error) {
	var out *public.RoleBinding
	err := s.command(ctx, plan, false, func(ctx context.Context, tx *sqlite.CommandTx, before *public.RoleBinding, at time.Time) error {
		desired := plan.Desired()
		if desired == nil {
			return objects.ErrSourceChanged
		}
		opt := store.ApplyOptions{Manager: plan.Manager(), Force: plan.Force(), CreateOnly: before == nil}
		if before != nil {
			// Keep lifecycle identity from the EXACT reviewed row. The manifest
			// retains its ordinary desired metadata, including finalizers.
			desired.UID = before.UID
			desired.ResourceVersion = before.ResourceVersion
			desired.CreationTimestamp = before.CreationTimestamp
			desired.Generation = before.Generation
			desired.DeletionTimestamp = before.DeletionTimestamp.DeepCopy()
			desired.DeletionGracePeriodSeconds = before.DeletionGracePeriodSeconds
			opt.ExpectedResourceVersion = before.ResourceVersion
		}
		written, err := security.Apply(ctx, tx, desired, opt, at)
		if err != nil {
			return err
		}
		var ok bool
		out, ok = written.(*public.RoleBinding)
		if !ok || out == nil {
			return objects.ErrSourceChanged
		}
		return nil
	})
	return out, err
}

func (s *sqliteObjectSources) DeleteRoleBinding(ctx context.Context, plan objects.RoleBindingPlan) error {
	return s.command(ctx, plan, true, func(ctx context.Context, tx *sqlite.CommandTx, before *public.RoleBinding, at time.Time) error {
		if before == nil {
			return objects.ErrSourceChanged
		}
		return security.Delete(ctx, tx, before, store.DeleteOptions{Manager: plan.Manager()}, at)
	})
}

func (s *sqliteObjectSources) command(ctx context.Context, plan objects.RoleBindingPlan, deleting bool, fn func(context.Context, *sqlite.CommandTx, *public.RoleBinding, time.Time) error) error {
	if s == nil || s.store == nil || s.now == nil || ctx == nil || fn == nil || plan.Deleting() != deleting {
		return objects.ErrSourceChanged
	}
	// Never let Store.Command's ordinary writeContext adopt a later writer.
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok {
		return replication.ErrFenced
	}
	expected := plan.Before()
	return plan.WithCurrent(ctx, func(current func() bool) error {
		return s.store.Command(ctx, token.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
			// This observation is AFTER actual writer-lock acquisition. A file
			// read's earlier timestamp must not undercut a racing enrollment.
			at := s.now().UTC()
			if at.IsZero() || plan.ObservedAt().IsZero() || at.Before(plan.ObservedAt()) || !current() {
				return objects.ErrSourceChanged
			}
			if err := tx.RequireCommitCheck(func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				final := s.now().UTC()
				if final.IsZero() || final.Before(at) || !current() {
					return objects.ErrSourceChanged
				}
				return nil
			}); err != nil {
				return err
			}
			var actual public.RoleBinding
			err := tx.Get(ctx, plan.Key(), &actual)
			if expected == nil {
				if err == nil {
					return store.ErrConflict
				}
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
				return fn(ctx, tx, nil, at)
			}
			if errors.Is(err, store.ErrNotFound) {
				return store.ErrConflict
			}
			if err != nil {
				return err
			}
			if actual.UID != expected.UID || actual.ResourceVersion != expected.ResourceVersion || store.ManagedBy(&actual) != store.ManagedBy(expected) {
				return store.ErrConflict
			}
			return fn(ctx, tx, &actual, at)
		})
	})
}

var _ objects.RoleBindingEffects = (*sqliteObjectSources)(nil)
