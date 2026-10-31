package unblock

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// JanitorInterval is how often the leader's janitor runs.
const JanitorInterval = time.Minute

// SystemActor decides expiries.
const SystemActor = "system"

// RunJanitor runs Sweep every JanitorInterval until ctx ends (leader
// only).
func (s *Service) RunJanitor(ctx context.Context) error {
	return s.RunCurrentJanitor(ctx, s.Sweep)
}

// RunCurrentJanitor runs the mounted atomic system sweep on the leader.
func (s *Service) RunCurrentJanitor(ctx context.Context, sweep func(context.Context) (SweepResult, error)) error {
	t := time.NewTicker(JanitorInterval)
	defer t.Stop()
	for {
		if _, err := sweep(ctx); err != nil && ctx.Err() == nil {
			s.opts.Logger.Warn("unblock request sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// SweepResult counts what one Sweep did.
type SweepResult struct {
	// Ended are time-limited approvals whose entry was released, Expired
	// pending requests nobody decided in time, Pruned deleted requests.
	Ended, Expired, Pruned int
}

// Sweep ends the time-limited approvals that are due, expires pending
// requests older than unblockRequests.pendingExpiry and deletes decided
// and expired requests older than unblockRequests.retention. It reads the
// store, not the index, so it works before the watch has synced.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	var all []*v1alpha1.UnblockRequest
	if err := s.each(ctx, Kind, "", func(o store.Object) {
		if r, ok := o.(*v1alpha1.UnblockRequest); ok {
			all = append(all, r)
		}
	}); err != nil {
		return res, err
	}
	set := s.Settings()
	now := s.now()
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	var errs []error
	for _, r := range all {
		switch {
		case r.EffectiveState() == v1alpha1.UnblockApproved && r.Status.Allow != nil && r.Status.Allow.Removed == nil &&
			r.Status.Allow.ExpiresAt != nil && !r.Status.Allow.ExpiresAt.After(now):
			if err := s.release(ctx, r, now); err != nil {
				errs = append(errs, err)
				continue
			}
			st := *r.Status.DeepCopy()
			st.State = v1alpha1.UnblockExpired
			st.Allow.Removed = &at
			if _, err := s.setStatus(ctx, r, st); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Ended++
			s.opts.Logger.Info("time-limited unblock approval ended", "request", r.Namespace+"/"+r.Name, "domain", r.Spec.Domain, "list", r.Status.Allow.List)
		case r.EffectiveState() == v1alpha1.UnblockPending && now.Sub(RequestedAt(r)) >= set.PendingExpiry.Std():
			st := *r.Status.DeepCopy()
			st.State, st.DecidedBy, st.DecidedAt = v1alpha1.UnblockExpired, SystemActor, &at
			st.Note = "No administrator decided within " + set.PendingExpiry.Std().String() + "."
			if _, err := s.setStatus(ctx, r, st); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Expired++
		case r.EffectiveState() != v1alpha1.UnblockPending && (!Active(r, now) || r.Status.Allow.ExpiresAt == nil):
			if decided := decidedAt(r); !decided.IsZero() && now.Sub(decided) >= set.Retention.Std() {
				err := s.opts.Store.Delete(ctx, keyOf(r.Namespace, r.Name), store.DeleteOptions{Manager: Manager, Force: true})
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					errs = append(errs, err)
					continue
				}
				s.mu.Lock()
				delete(s.items, keyOf(r.Namespace, r.Name))
				s.mu.Unlock()
				res.Pruned++
			}
		}
	}
	return res, errors.Join(errs...)
}

// decidedAt is when r was last decided or its entry removed.
func decidedAt(r *v1alpha1.UnblockRequest) time.Time {
	var t time.Time
	if r.Status.DecidedAt != nil {
		t = r.Status.DecidedAt.Time
	}
	if a := r.Status.Allow; a != nil && a.Removed != nil && a.Removed.After(t) {
		t = a.Removed.Time
	}
	return t
}
