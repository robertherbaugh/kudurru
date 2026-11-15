package host

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestProviderRequestsBindOperationCaptureAndChargeAttempts(t *testing.T) {
	h, j, _, st, _ := engineFixture(t)
	p := providerHost{h, j}
	_, g, _ := h.captured(j)
	ctx := scoped(context.Background(), j, g)
	called := 0
	request := func(context.Context) error { called++; return nil }
	if err := p.Request(context.Background(), "cloudflare", "token-a", true, request); !errors.Is(err, ErrFenced) {
		t.Fatal("unscoped background request allowed", err)
	}
	for _, account := range []string{"token-a", "token-b"} {
		if err := p.Request(ctx, "cloudflare", account, true, request); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.List(ctx, "IntegrationBudget", store.ListOptions{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal("rotation split shared budget", page, err)
	}
	h.mu.Lock()
	j.frame.input.BundleID = "next"
	h.mu.Unlock()
	if err := p.Request(ctx, "cloudflare", "token-c", true, request); !errors.Is(err, ErrFenced) || called != 2 {
		t.Fatal("old operation rebound to new source bundle", called, err)
	}
	// Every HTTP retry reenters the host; failed storage never loans a request.
	_, next, _ := h.captured(j)
	ctx = scoped(context.Background(), j, next)
	repo, err := ledger.New(ledger.Options{Reader: st, Write: func(context.Context, ledger.Guard, store.Object, store.ApplyOptions) (store.Object, error) {
		return nil, store.ErrClosed
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.Ledger = repo
	if err := p.Request(ctx, "cloudflare", "all", false, request); !errors.Is(err, store.ErrClosed) || called != 2 {
		t.Fatal("budget failure loaned request", called, err)
	}
}
func TestProviderRequestDeadlineAndLeaseLoss(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	h.opts.OperationTimeout = 20 * time.Millisecond
	_, g, _ := h.captured(j)
	p := providerHost{h, j}
	err := p.Request(scoped(context.Background(), j, g), "local", "account", false, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	j.cancel()
	if err := p.Request(scoped(context.Background(), j, g), "local", "account", false, func(context.Context) error { t.Fatal("lost lease loaned request"); return nil }); !errors.Is(err, ErrFenced) {
		t.Fatal(err)
	}
}

func TestProviderRequestRechecksAfterBudgetReservation(t *testing.T) {
	for _, mutating := range []bool{false, true} {
		name := "read"
		if mutating {
			name = "write"
		}
		t.Run(name, func(t *testing.T) {
			h, j, _, st, _ := engineFixture(t)
			_, g, err := h.captured(j)
			if err != nil {
				t.Fatal(err)
			}
			reserved := false
			repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
				out, err := st.Apply(ctx, obj, opts)
				if err == nil {
					reserved = true
					h.mu.Lock()
					j.frame.input.BundleID = "replaced-during-reservation"
					h.mu.Unlock()
				}
				return out, err
			}})
			if err != nil {
				t.Fatal(err)
			}
			h.opts.Ledger = repo
			called := false
			err = (providerHost{h, j}).Request(scoped(t.Context(), j, g), "local", "account", mutating, func(context.Context) error {
				called = true
				return nil
			})
			if !reserved || called || !errors.Is(err, ErrFenced) {
				t.Fatal("reservation allowed a request under replaced authority", reserved, called, err)
			}
		})
	}
}

func TestRequestGuardDoesNotAdoptSuccessor(t *testing.T) {
	h, j, _ := preferredSourceFixture(t)
	f, g, err := h.captured(j)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := h.scopedOriginal(t.Context(), j, f, g)
	if err != nil {
		t.Fatal(err)
	}
	if got, lease, err := h.RequestGuard(ctx, g.Ledger); err != nil || got != g || lease != j.leaseCtx {
		t.Fatal("original request guard unavailable", err)
	}
	h.mu.Lock()
	j.frame.input.BundleID = "successor"
	h.mu.Unlock()
	if _, _, err := h.RequestGuard(ctx, g.Ledger); err != ErrFenced {
		t.Fatal("successor repaired original guard")
	}
	if _, _, err := h.RequestGuard(t.Context(), g.Ledger); err != ErrFenced {
		t.Fatal("unscoped request acquired authority")
	}
}

func TestOriginalResourceSurvivesEquivalentAuthorityRefresh(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	original, successor := new(int), new(int)
	j.frame.input.Resource = original
	encoded, err := json.Marshal(j.frame.input.Sources)
	if err != nil {
		t.Fatal(err)
	}
	j.sourceDigest = kinds.Digest(encoded)
	f, g, err := h.captured(j)
	if err != nil {
		t.Fatal(err)
	}
	held, err := h.scopedOriginal(t.Context(), j, f, g)
	if err != nil {
		t.Fatal(err)
	}
	next := f.input
	next.Resource = successor
	h.opts.Capture = func(context.Context) (Input, error) { return next, nil }
	h.refresh(t.Context())
	got, err := h.RequestResource(held, g.Ledger)
	if err != nil || got != original {
		t.Fatal("equivalent refresh replaced original resource", err)
	}
	fresh, freshGuard, err := h.captured(j)
	if err != nil {
		t.Fatal(err)
	}
	future, err := h.scopedOriginal(t.Context(), j, fresh, freshGuard)
	if err != nil {
		t.Fatal(err)
	}
	got, err = h.RequestResource(future, freshGuard.Ledger)
	if err != nil || got != successor {
		t.Fatal("future request did not capture refreshed resource", err)
	}
	h.mu.Lock()
	replacement := *j.frame.instance
	j.frame.instance = &replacement
	h.mu.Unlock()
	if _, err = h.RequestResource(held, g.Ledger); !errors.Is(err, ErrFenced) {
		t.Fatal("same-guard replacement repaired held resource", err)
	}
}

// A lost budget CAS (another instance reserved first; nothing was written)
// is followed by fresh reservation attempts, each fenced again, until one is
// written or the request's own context ends (OperationTimeout, lease or job);
// the request runs only once one is written. Other failures, and a change of
// authority between attempts, end the request with no I/O (D364, D384).
func TestProviderRequestReservesAgainAfterLostBudgetCAS(t *testing.T) {
	h, j, _, st, _ := engineFixture(t)
	p := providerHost{h, j}
	_, g, _ := h.captured(j)
	ctx := scoped(context.Background(), j, g)
	var mu sync.Mutex
	var writes, lose int
	repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		mu.Lock()
		writes++
		lost := writes <= lose
		mu.Unlock()
		if lost {
			return nil, store.ErrConflict
		}
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.Ledger = repo
	count := func() int { mu.Lock(); defer mu.Unlock(); return writes }
	set := func(w, l int) { mu.Lock(); writes, lose = w, l; mu.Unlock() }
	called := 0
	request := func(context.Context) error { called++; return nil }
	// Many instances reserving together: a request that loses many rounds in
	// a row still reserves and runs, with no fixed attempt bound.
	set(0, 12)
	if err := p.Request(ctx, "cloudflare", "all", true, request); err != nil || called != 1 || count() != 13 {
		t.Fatal("lost budget CAS was not reserved again", err, called, count())
	}
	// A CAS that keeps losing ends with the request's own deadline: no
	// unreserved request, and the growing pause bounds the attempts.
	h.opts.OperationTimeout = 300 * time.Millisecond
	set(0, 1<<30)
	start := time.Now()
	err = p.Request(ctx, "cloudflare", "all", true, request)
	took := time.Since(start)
	if !errors.Is(err, store.ErrConflict) || !errors.Is(err, context.DeadlineExceeded) || called != 1 || count() < 3 || count() > 12 || took < 250*time.Millisecond || took > 5*time.Second {
		t.Fatal("unbounded or unreserved request", err, called, count(), took)
	}
	// A fence failure between attempts ends the request.
	set(0, 1)
	fenced := 0
	h.opts.Fence = func(context.Context, ExecutionGuard) error {
		if count() >= 1 {
			fenced++
			return errors.New("lease lost")
		}
		return nil
	}
	if err := p.Request(ctx, "cloudflare", "all", true, request); !errors.Is(err, ErrFenced) || called != 1 || count() != 1 || fenced == 0 {
		t.Fatal("re-reservation skipped its fence", err, called, count())
	}
	// Canceling the job ends the attempts at once.
	h.opts.Fence = func(context.Context, ExecutionGuard) error { return nil }
	h.opts.OperationTimeout = time.Minute
	set(0, 1<<30)
	go func() {
		for count() < 2 {
			time.Sleep(time.Millisecond)
		}
		j.cancel()
	}()
	start = time.Now()
	err = p.Request(ctx, "cloudflare", "all", true, request)
	if err == nil || called != 1 || time.Since(start) > 5*time.Second {
		t.Fatal("canceled job kept reserving", err, called, count(), time.Since(start))
	}
	page, err := st.List(t.Context(), "IntegrationBudget", store.ListOptions{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal("budget", page, err)
	}
	var budget *kinds.IntegrationBudget
	if b, ok := page.Items[0].(*kinds.IntegrationBudget); ok {
		budget = b
	}
	if budget == nil || len(budget.Spec.Reservations) != 1 {
		t.Fatal("reservations", budget)
	}
}
