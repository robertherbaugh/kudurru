package host

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
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
	var attempts []time.Time
	repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		mu.Lock()
		writes++
		attempts = append(attempts, time.Now())
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
	set := func(w, l int) { mu.Lock(); writes, lose, attempts = w, l, nil; mu.Unlock() }
	observedAttempts := func() []time.Time { mu.Lock(); defer mu.Unlock(); return append([]time.Time(nil), attempts...) }
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
	observed := observedAttempts()
	pause := budgetPauseFirst
	for i := 1; i < len(observed); i++ {
		// Every new attempt follows jitter in the upper half of the current
		// pause. This relation, unlike a fixed attempt count, is independent
		// of how often the scheduler runs this goroutine before the deadline.
		if elapsed := observed[i].Sub(observed[i-1]); elapsed < pause/2 {
			t.Fatalf("reservation attempt %d followed after %s, want at least %s", i+1, elapsed, pause/2)
		}
		pause = min(2*pause, budgetPauseMax)
	}
	if !errors.Is(err, store.ErrConflict) || !errors.Is(err, context.DeadlineExceeded) || called != 1 || len(observed) == 0 || took < h.opts.OperationTimeout || took > 5*time.Second {
		t.Fatal("unbounded or unreserved request", err, called, len(observed), took)
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

type budgetContentionPublisher struct {
	*fakePublisher
	err error
}

func (p *budgetContentionPublisher) Records(context.Context, kinds.Destination) (RemoteSnapshot, error) {
	return RemoteSnapshot{}, p.err
}

// A budget CAS that keeps losing until the request deadline returns a joined
// store conflict and deadline error. Its consumers intentionally have distinct
// historical classifications. This test pins each classification without
// endorsing it and proves that none of them follows an unreserved request.
func TestJoinedBudgetContentionConsumerClassification(t *testing.T) {
	contention := func(t *testing.T) error {
		t.Helper()
		h, j, _, st, _ := engineFixture(t)
		h.opts.OperationTimeout = 40 * time.Millisecond
		var sent atomic.Int32
		repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, _ store.Object, _ store.ApplyOptions) (store.Object, error) {
			<-ctx.Done()
			return nil, store.ErrConflict
		}})
		if err != nil {
			t.Fatal(err)
		}
		h.opts.Ledger = repo
		_, g, err := h.captured(j)
		if err != nil {
			t.Fatal(err)
		}
		err = (providerHost{h: h, j: j}).Request(scoped(t.Context(), j, g), "test-provider", "account", false, func(context.Context) error {
			sent.Add(1)
			return nil
		})
		if !errors.Is(err, store.ErrConflict) || !errors.Is(err, context.DeadlineExceeded) || sent.Load() != 0 {
			t.Fatalf("contention error=%v requests=%d", err, sent.Load())
		}
		return err
	}

	for _, tc := range []struct {
		name     string
		want     string
		classify func(*testing.T, error) string
	}{
		{
			name: "event",
			want: "local_refusal",
			classify: func(t *testing.T, err error) string {
				t.Helper()
				h, j, _, now := deliveryFixture(t)
				id := queueOne(t, h, j, *now, "budget-contention", &fakeHook{handle: func(context.Context, string, integration.Event) error {
					return err
				}})
				delivery, readErr := h.opts.Ledger.ReadDelivery(t.Context(), j.guard.Owner, id)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if delivery.Spec.Attempts == 0 && !delivery.Spec.Acknowledged && !delivery.Spec.DeadLetter && delivery.Spec.ErrorClass == "" {
					return "local_refusal"
				}
				return "peer_answer"
			},
		},
		{
			name: "cleanup",
			want: "frame_or_ledger_moved",
			classify: func(t *testing.T, err error) string {
				t.Helper()
				if cleanupFrameMoved(err) {
					return "frame_or_ledger_moved"
				}
				return "other_failure"
			},
		},
		{
			name: "sync",
			want: "ReconcileFailed/provider",
			classify: func(t *testing.T, err error) string {
				t.Helper()
				h, j, p, _, _ := engineFixture(t)
				j.provider = &budgetContentionPublisher{fakePublisher: p, err: err}
				answer := h.execute(t.Context(), j, command{kind: "sync"})
				statuses := h.Statuses()
				if !errors.Is(answer.err, ErrProvider) || len(statuses) != 1 || p.calls != 0 {
					t.Fatalf("sync error=%v statuses=%+v requests=%d", answer.err, statuses, p.calls)
				}
				return statuses[0].Reason + "/provider"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.classify(t, contention(t)); got != tc.want {
				t.Fatalf("classification %q, want %q", got, tc.want)
			}
		})
	}
}
