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

// D408: the next periodic poll begins one interval after the previous poll
// began, not one interval after its source commit acknowledged. An
// observation's evidence lives a fixed multiple of the interval from the
// observation (UniFi: three polls), so a slow acknowledgment (the complete
// publication a loaded node prepares) must not push the next observation
// later. A poll that runs longer than the interval is followed at once, never
// overlapped.
func TestPeriodicPollIntervalCountsFromThePreviousPoll(t *testing.T) {
	const interval = 2 * time.Second
	// The first acknowledgment is shorter than the interval, the second longer.
	acks := []time.Duration{1500 * time.Millisecond, 3 * time.Second}
	st := memory.New()
	definition := webhook("cadence")
	definition.Spec.Resync = metav1.Duration{Duration: interval}
	input := captureInput(t, definition)
	repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, obj store.Object, options store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, obj, options)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var observed []time.Time
	commits := 0
	p := sourceOnlyObserver{Integration: &fakePublisher{}, observe: func(context.Context) (SourceSnapshot, error) {
		mu.Lock()
		observed = append(observed, time.Now())
		mu.Unlock()
		return SourceSnapshot{Complete: true}, nil
	}}
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return input, nil }, Factory: func(string) (integration.Integration, error) { return p, nil },
		Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: repo, Reader: st, Allowed: func() bool { return true },
		BeforeObserve: func(ctx, _ context.Context, _ ExecutionGuard) (context.Context, func(), error) {
			return ctx, func() {}, nil
		},
		ValidateSource: func(ExecutionGuard, SourceSnapshot) error { return nil },
		CommitSource: func(ctx context.Context, _ ExecutionGuard, s SourceSnapshot) (*Result, error) {
			mu.Lock()
			ack := time.Duration(0)
			if commits < len(acks) {
				ack = acks[commits]
			}
			commits++
			mu.Unlock()
			// The commit acknowledges only after its publication is prepared.
			select {
			case <-time.After(ack):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &Result{Clients: len(s.Clients), Source: &SourceOutcome{ReceiptID: "receipt", Committed: true, Published: true}}, nil
		},
		PollInterval: 10 * time.Millisecond, OperationTimeout: 10 * time.Second, StopTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("periodic source did not join")
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(observed)
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic polls stopped after %d observations", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	first, second := observed[1].Sub(observed[0]), observed[2].Sub(observed[1])
	mu.Unlock()
	// Counted from the acknowledgment, the gaps would be interval+acks[0]
	// (3.5 s) and interval+acks[1] (5 s). Each bound below leaves one second
	// for scheduling on a loaded race run.
	if first < interval-50*time.Millisecond || first >= interval+time.Second {
		t.Fatalf("poll after a %s acknowledgment began %s after the previous one, want %s", acks[0], first, interval)
	}
	if second < acks[1] || second >= acks[1]+time.Second {
		t.Fatalf("poll after a %s acknowledgment began %s after the previous one, want at once after it (%s)", acks[1], second, acks[1])
	}
}

// D408: a sync ends with the job's own status write, which gives the stored
// instance a resource version its frame lacks until the host captures it
// again; root fences a sync on the older frame. When the next periodic sync
// is due at once (the previous one ran longer than the interval), it waits
// for a capture begun after that write, which the job requests, instead of
// running on the old frame and being fenced until the next interval.
func TestPeriodicPollAfterStatusWriteUsesAFrameCapturedAfterIt(t *testing.T) {
	const interval = time.Second
	st := memory.New()
	definition := webhook("status-frame")
	definition.Spec.Resync = metav1.Duration{Duration: interval}
	input := captureInput(t, definition)
	repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, obj store.Object, options store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, obj, options)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	written := 0   // status writes so far
	var seen []int // status writes the frame of each observation had captured
	var h *Host
	p := sourceOnlyObserver{Integration: &fakePublisher{}, observe: func(ctx context.Context) (SourceSnapshot, error) {
		scope := ctx.Value(scopeKey{}).(requestScope)
		resource, err := h.RequestResource(ctx, scope.g.Ledger)
		if err != nil {
			return SourceSnapshot{}, err
		}
		mu.Lock()
		seen = append(seen, *resource.(*int))
		mu.Unlock()
		return SourceSnapshot{Complete: true}, nil
	}}
	h, err = New(Options{Capture: func(context.Context) (Input, error) {
		mu.Lock()
		defer mu.Unlock()
		captured := written
		next := input
		next.Resource = &captured
		return next, nil
	}, Factory: func(string) (integration.Integration, error) { return p, nil },
		Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: repo, Reader: st, Allowed: func() bool { return true },
		BeforeObserve: func(ctx, _ context.Context, _ ExecutionGuard) (context.Context, func(), error) {
			return ctx, func() {}, nil
		},
		ValidateSource: func(ExecutionGuard, SourceSnapshot) error { return nil },
		CommitSource: func(ctx context.Context, _ ExecutionGuard, s SourceSnapshot) (*Result, error) {
			// The acknowledgment outlasts the interval, so the next sync is due at once.
			select {
			case <-time.After(interval + 200*time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &Result{Clients: len(s.Clients), Source: &SourceOutcome{ReceiptID: "receipt", Committed: true, Published: true}}, nil
		},
		Status: func(context.Context, ExecutionGuard, InstanceStatus) error {
			mu.Lock()
			written++
			mu.Unlock()
			return nil
		},
		// Only a wake refreshes frames within this test's run.
		PollInterval: time.Minute, OperationTimeout: 10 * time.Second, StopTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("periodic source did not join")
		}
	}()
	// The first sync runs on the job's first frame; its election and
	// provisioning write status before it, which the frame may not have.
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n, writes := len(seen), written
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic polls stopped after %d observations and %d status writes", n, writes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// Each sync after the first must run on a frame captured after the
	// status write that ended the sync before it.
	for i := 1; i < 3; i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("observation %d ran on a frame captured before the previous sync's status write: %v", i+1, seen)
		}
	}
}
