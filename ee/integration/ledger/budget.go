package ledger

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrBudget means no external request may be made until a fresh reservation.
var ErrBudget = errors.New("integration ledger: request budget exhausted")

// BudgetCharge is one idempotent request reservation in a shared account window.
// Account is provider identity or a conservative all-accounts bucket, not a
// credential fingerprint. Credential rotation must never reset request budgets.
type BudgetCharge struct {
	Provider, Account, ID         string
	Cost, Capacity, WindowSeconds uint32
}

// ReserveBudget uses exact CAS once; conflicts require a fresh explicit
// reservation attempt, never an unfenced external request. Unused reservations
// are retained conservatively. Backward clock motion never opens a new window.
func (r *Repository) ReserveBudget(ctx context.Context, g Guard, c BudgetCharge) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if g.Validate() != nil || !kinds.ValidDigest(c.ID) || c.Cost == 0 || c.Cost > c.Capacity || c.Capacity > 100000 || c.WindowSeconds == 0 || c.WindowSeconds > 3600 {
		return time.Time{}, ErrInvalid
	}
	now := r.now().UTC()
	if now.IsZero() {
		return time.Time{}, ErrInvalid
	}
	name := kinds.BudgetName(g.Owner.ClusterID, c.Provider, c.Account)
	b := &kinds.IntegrationBudget{}
	err := r.read.Get(ctx, store.Key{Kind: kinds.KindIntegrationBudget, Name: name}, b)
	create := errors.Is(err, store.ErrNotFound)
	if err != nil && !create {
		return time.Time{}, err
	}
	if create {
		b = &kinds.IntegrationBudget{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: kinds.IntegrationBudgetSpec{FormatVersion: 1, ClusterID: g.Owner.ClusterID, Provider: c.Provider, Account: c.Account, Sequence: 1, WindowStart: now, WindowSeconds: c.WindowSeconds, Capacity: c.Capacity}}
	} else {
		if b.Validate() != nil || b.ResourceVersion == "" || b.Spec.ClusterID != g.Owner.ClusterID || b.Spec.Provider != c.Provider || b.Spec.Account != c.Account {
			return time.Time{}, ErrInvalid
		}
		until := b.Spec.WindowStart.Add(time.Duration(b.Spec.WindowSeconds) * time.Second)
		if now.Before(b.Spec.WindowStart) {
			return until, ErrBudget
		}
		if !now.Before(until) {
			b.Spec.WindowStart = now
			b.Spec.WindowSeconds = c.WindowSeconds
			b.Spec.Capacity = c.Capacity
			b.Spec.Reservations = nil
		} else if b.Spec.WindowSeconds != c.WindowSeconds || b.Spec.Capacity != c.Capacity {
			return until, ErrInvalid
		}
	}
	until := b.Spec.WindowStart.Add(time.Duration(b.Spec.WindowSeconds) * time.Second)
	used := uint64(0)
	for _, v := range b.Spec.Reservations {
		if v.ID == c.ID {
			if v.Cost != c.Cost {
				return until, ErrInvalid
			}
			return until, nil
		}
		used += uint64(v.Cost)
	}
	if used+uint64(c.Cost) > uint64(b.Spec.Capacity) {
		return until, ErrBudget
	}
	if !create {
		if b.Spec.Sequence == math.MaxUint64 {
			return until, ErrOverflow
		}
		b.Spec.Sequence++
	}
	b.Spec.Reservations = append(b.Spec.Reservations, kinds.BudgetReservation{ID: c.ID, Cost: c.Cost})
	if b.Validate() != nil {
		return until, ErrInvalid
	}
	out, err := r.write(ctx, g, b.DeepCopy(), store.ApplyOptions{Manager: manager, ExpectedResourceVersion: b.ResourceVersion, CreateOnly: create})
	if err != nil {
		return until, err
	}
	saved, ok := out.(*kinds.IntegrationBudget)
	if !ok || saved.Validate() != nil || saved.ResourceVersion == "" || !reflect.DeepEqual(saved.Spec, b.Spec) {
		return until, ErrInvalid
	}
	return until, nil
}
