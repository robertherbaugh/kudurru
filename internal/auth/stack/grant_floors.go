package stack

import (
	"context"
	"sync"
	"time"

	eerbac "github.com/robertherbaugh/kudurru/ee/rbac"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// GrantFloorStore persists RoleBinding revocation floors shared by every
// replica (the Kubernetes store's kudurru-grant-floors Secret).
type GrantFloorStore interface {
	ReadGrantFloors(context.Context) (map[string]time.Time, error)
	MergeGrantFloors(context.Context, map[string]time.Time) error
}

const grantFloorInterval = 5 * time.Second

// grantFloors queues revocations this replica observes and periodically
// merges them into the durable record, then loads every replica's floors.
type grantFloors struct {
	store    GrantFloorStore
	bindings *eerbac.BindingSource
	mu       sync.Mutex
	pending  map[string]time.Time
	wake     chan struct{}
}

func newGrantFloors(st GrantFloorStore, b *eerbac.BindingSource) *grantFloors {
	f := &grantFloors{store: st, bindings: b, pending: map[string]time.Time{}, wake: make(chan struct{}, 1)}
	b.OnRevoke(func(revoked []eerbac.Revocation) {
		f.mu.Lock()
		for _, r := range revoked {
			key := r.Grant.Normalized().String()
			if old, ok := f.pending[key]; !ok || r.At.After(old) {
				f.pending[key] = r.At
			}
		}
		f.mu.Unlock()
		select {
		case f.wake <- struct{}{}:
		default:
		}
	})
	return f
}

// sync persists pending revocations, then loads and merges all floors. A
// failed persist keeps the revocations pending (they stay in force in memory).
func (f *grantFloors) sync(ctx context.Context) error {
	f.mu.Lock()
	pending := make(map[string]time.Time, len(f.pending))
	for k, v := range f.pending {
		pending[k] = v
	}
	f.mu.Unlock()
	if len(pending) > 0 {
		if err := f.store.MergeGrantFloors(ctx, pending); err != nil {
			return err
		}
		f.mu.Lock()
		for k, v := range pending {
			if cur, ok := f.pending[k]; ok && !cur.After(v) {
				delete(f.pending, k)
			}
		}
		f.mu.Unlock()
	}
	raw, err := f.store.ReadGrantFloors(ctx)
	if err != nil {
		return err
	}
	floors := make(map[rbac.Grant]time.Time, len(raw))
	for key, at := range raw {
		g, err := rbac.ParseGrant(key)
		if err != nil {
			continue
		}
		floors[g.Normalized()] = at
	}
	f.bindings.MergeFloors(floors)
	return nil
}

func (f *grantFloors) run(ctx context.Context) {
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = f.sync(attempt)
		cancel()
		timer := time.NewTimer(grantFloorInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-f.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
