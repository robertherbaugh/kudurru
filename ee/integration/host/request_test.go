package host

import (
	"context"
	"errors"
	"testing"
	"time"

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
