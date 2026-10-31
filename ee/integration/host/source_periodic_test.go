package host

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInventoryPeriodicPollAfterOriginalFrameFence(t *testing.T) {
	for _, mode := range []string{"future_capture", "changed_uid", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			st := memory.New()
			definition := webhook("inventory")
			definition.Spec.Resync = metav1.Duration{Duration: time.Second}
			input := captureInput(t, definition)
			original, successor := new(int), new(int)
			input.Resource = original
			nextDefinition := definition.DeepCopy()
			nextDefinition.UID = "successor-source-uid"
			nextInput := captureInput(t, nextDefinition)
			nextInput.BundleID, nextInput.Resource = "successor-source-cut", successor
			var cancel context.CancelFunc
			var factories atomic.Int32
			var mu sync.RWMutex
			repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, obj store.Object, options store.ApplyOptions) (store.Object, error) {
				return st.Apply(ctx, obj, options)
			}})
			if err != nil {
				t.Fatal(err)
			}
			var h *Host
			var calls atomic.Int32
			var first context.Context
			p := sourceOnlyObserver{Integration: &fakePublisher{}, observe: func(ctx context.Context) (SourceSnapshot, error) {
				count := calls.Add(1)
				scope := ctx.Value(scopeKey{}).(requestScope)
				resource, err := h.RequestResource(ctx, scope.g.Ledger)
				if err != nil {
					t.Error("missing original poll resource")
					return SourceSnapshot{}, err
				}
				if count == 1 {
					first = ctx
					if resource != original {
						t.Error("first poll borrowed successor resource")
					}
					if mode == "canceled" {
						cancel()
					} else {
						mu.Lock()
						if mode == "changed_uid" {
							input = nextInput
						} else {
							input.BundleID, input.Resource = "successor-source-cut", successor
						}
						mu.Unlock()
						// Deterministic replacement during acquisition, without a sleep.
						h.refresh(ctx)
					}
				} else if resource != successor {
					t.Error("future periodic poll did not capture its own resource")
				}
				return SourceSnapshot{Complete: true}, nil
			}}
			h, err = New(Options{Capture: func(context.Context) (Input, error) { mu.RLock(); defer mu.RUnlock(); return input, nil }, Factory: func(string) (integration.Integration, error) {
				factories.Add(1)
				provider := p
				provider.Integration = &fakePublisher{}
				return provider, nil
			}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: repo, Reader: st, Inventory: inventory.New(nil), Allowed: func() bool { return true }, PollInterval: time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithCancel(t.Context())
			cancel = stop
			done := make(chan error, 1)
			go func() { done <- h.Run(ctx) }()
			joined := false
			t.Cleanup(func() {
				if joined {
					return
				}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("periodic source did not join")
				}
			})
			if mode == "canceled" {
				select {
				case err := <-done:
					joined = true
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("canceled source failed to join")
				}
				if calls.Load() != 1 || factories.Load() != 1 || h.opts.Inventory.Snapshot() != nil {
					t.Fatal("canceled old poll published or scheduled another acquisition")
				}
				return
			}
			waitFor(t, func() bool { return h.opts.Inventory.Snapshot() != nil })
			if calls.Load() != 2 || h.opts.Inventory.Snapshot().Generation() != 1 {
				t.Fatal("refused original poll published, or future periodic acquisition stopped")
			}
			if mode == "changed_uid" && factories.Load() != 2 {
				t.Fatal("changed source resumed its old provider instead of joining and replacing it")
			}
			if mode == "future_capture" && factories.Load() != 1 {
				t.Fatal("incidental frame refresh restarted the provider")
			}
			if first == nil || !errors.Is(first.Err(), context.Canceled) {
				t.Fatal("original poll context survived as a future request")
			}

		})
	}
}
