package ledger

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func charge(id string) BudgetCharge {
	return BudgetCharge{Provider: "cloudflare", Account: "all", ID: kinds.Digest([]byte(id)), Cost: 1, Capacity: 2, WindowSeconds: 300}
}
func TestBudgetSharingRotationReplayAndWindow(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g, _ := fixture(t)
	now := testTime
	r, err := New(Options{Reader: st, Now: func() time.Time { return now }, Write: func(ctx context.Context, _ Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	until, err := r.ReserveBudget(ctx, g, charge("a"))
	if err != nil || !until.Equal(now.Add(300*time.Second)) {
		t.Fatal(until, err)
	}
	// Another node/instance and a rotated credential still share the same account.
	other := g
	other.Owner.UID = "uid-2"
	other.Owner.Name = "other"
	other.LeaseID = "lease-2"
	other.CredentialDigest = kinds.Digest([]byte("rotated"))
	if _, err := r.ReserveBudget(ctx, other, charge("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReserveBudget(ctx, g, charge("a")); err != nil {
		t.Fatal("replay charged twice", err)
	}
	c := charge("a")
	c.Cost = 2
	if _, err := r.ReserveBudget(ctx, g, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("changed replay cost", err)
	}
	c = charge("c")
	if _, err := r.ReserveBudget(ctx, g, c); !errors.Is(err, ErrBudget) {
		t.Fatal("exceeded shared window", err)
	}
	now = until.Add(-time.Nanosecond)
	if _, err := r.ReserveBudget(ctx, g, c); !errors.Is(err, ErrBudget) {
		t.Fatal("early reset", err)
	}
	now = until
	if _, err := r.ReserveBudget(ctx, g, c); err != nil {
		t.Fatal("boundary did not reset", err)
	}
	now = testTime
	if next, err := r.ReserveBudget(ctx, g, charge("backwards")); !errors.Is(err, ErrBudget) || !next.Equal(until.Add(300*time.Second)) {
		t.Fatal("backward clock reset window", next, err)
	}
}
func TestBudgetCASFencingStorageAndOutput(t *testing.T) {
	ctx := context.Background()
	g, _ := fixture(t)
	for _, tc := range []struct {
		name   string
		mutate func(store.Object) store.Object
		fail   error
		want   error
	}{
		{name: "storage", fail: store.ErrClosed, want: store.ErrClosed},
		{name: "fence", fail: ErrPending, want: ErrPending},
		{name: "wrong result", mutate: func(o store.Object) store.Object {
			b := o.(*kinds.IntegrationBudget).DeepCopy()
			b.Spec.Capacity++
			b.ResourceVersion = "1"
			return b
		}, want: ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := memory.New()
			r := repository(t, st, func(_ context.Context, guard Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
				if guard != g || !a.CreateOnly || a.ExpectedResourceVersion != "" || a.Force {
					t.Fatal("lost fence/CAS")
				}
				if tc.fail != nil {
					return nil, tc.fail
				}
				return tc.mutate(o), nil
			})
			if _, err := r.ReserveBudget(ctx, g, charge("x")); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
	st := memory.New()
	r := repository(t, st, nil)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			_, err := r.ReserveBudget(ctx, g, charge(strconv.Itoa(i)))
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, store.ErrConflict) && !errors.Is(err, ErrBudget) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if winners.Load() < 1 || winners.Load() > 2 {
		t.Fatal("concurrent capacity", winners.Load())
	}
	// Failure is never retried with a freshly substituted token.
	reads := 0
	r = repository(t, st, func(context.Context, Guard, store.Object, store.ApplyOptions) (store.Object, error) {
		reads++
		return nil, store.ErrConflict
	})
	c := charge("conflict")
	c.Account = "other"
	if _, err := r.ReserveBudget(ctx, g, c); !errors.Is(err, store.ErrConflict) || reads != 1 {
		t.Fatal("CAS retried", reads, err)
	}
}
func TestBudgetValidationOverflowAndCanceled(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	r := repository(t, st, nil)
	g, _ := fixture(t)
	for _, change := range []func(*BudgetCharge){func(c *BudgetCharge) { c.Provider = "" }, func(c *BudgetCharge) { c.Account = "" }, func(c *BudgetCharge) { c.ID = "bad" }, func(c *BudgetCharge) { c.Cost = 0 }, func(c *BudgetCharge) { c.Capacity = 0 }, func(c *BudgetCharge) { c.WindowSeconds = 3601 }} {
		c := charge("x")
		change(&c)
		if _, err := r.ReserveBudget(ctx, g, c); !errors.Is(err, ErrInvalid) {
			t.Fatal(c, err)
		}
	}
	if _, err := r.ReserveBudget(ctx, g, charge("a")); err != nil {
		t.Fatal(err)
	}
	b := &kinds.IntegrationBudget{}
	if err := st.Get(ctx, store.Key{Kind: kinds.KindIntegrationBudget, Name: kinds.BudgetName(g.Owner.ClusterID, "cloudflare", "all")}, b); err != nil {
		t.Fatal(err)
	}
	b.Spec.Sequence = math.MaxUint64
	if _, err := st.Apply(ctx, b, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: b.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReserveBudget(ctx, g, charge("b")); !errors.Is(err, ErrOverflow) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.ReserveBudget(canceled, g, charge("a")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
