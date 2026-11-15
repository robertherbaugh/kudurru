package host

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// liveCaptures follows one memory store through a zone cache, so successive
// captures carry the resource versions the store assigned to each write.
func liveCaptures(t *testing.T, st *memory.Store) func(rv string) Input {
	t.Helper()
	cache, err := zonecache.New(st, zonecache.Options{Kinds: []store.Kind{"Integration"}, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var latest atomic.Pointer[zonecache.ObjectSet]
	cache.OnPublish(func(s *zonecache.Snapshot) { objects := s.Objects; latest.Store(&objects) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cache.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return func(rv string) Input {
		t.Helper()
		var objects zonecache.ObjectSet
		waitFor(t, func() bool {
			p := latest.Load()
			if p == nil {
				return false
			}
			o, ok := p.Get(store.Key{Kind: "Integration", Namespace: "dns", Name: "sink"})
			if !ok || o.GetResourceVersion() != rv {
				return false
			}
			objects = *p
			return true
		})
		return Input{Objects: objects, Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}, BundleID: "bundle-1", Complete: true}
	}
}

// TestStatusOnlyWriteKeepsStatusAndReviewCurrent proves D384: a status write
// on a running instance's own object neither leaves its local status on the
// old revision (statusKnown false in the admin API) nor makes a command
// reviewed at that revision stale, while any other change still does.
func TestStatusOnlyWriteKeepsStatusAndReviewCurrent(t *testing.T) {
	st := memory.New()
	ctx := t.Context()
	created, err := st.Apply(ctx, webhook("sink"), store.ApplyOptions{Manager: "test", CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	captures := liveCaptures(t, st)
	rv1 := created.GetResourceVersion()
	var mu sync.RWMutex
	in := captures(rv1)
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := &countingPublisher{}
	h, err := New(Options{Capture: func(context.Context) (Input, error) { mu.RLock(); defer mu.RUnlock(); return in, nil }, Factory: func(string) (integration.Integration, error) { return p, nil }, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true }, PollInterval: time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	replace := func(next Input) { mu.Lock(); in = next; mu.Unlock(); h.Wake() }
	id := InstanceID{ClusterID: "cluster", Namespace: "dns", Name: "sink", UID: "uid-sink"}
	revision := func() string {
		for _, s := range h.Statuses() {
			if s.Owner == id {
				return s.Revision
			}
		}
		return ""
	}
	waitFor(t, func() bool { return p.plans.Load() == 1 })
	if _, err := h.CaptureCommand(ctx, id, rv1); err != nil {
		t.Fatalf("command at the running revision: %v", err)
	}

	// The host's own status write: a new resource version, nothing else.
	current := &public.Integration{}
	if err := st.Get(ctx, store.KeyOf(created), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	current.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synchronized", Message: "test", LastTransitionTime: metav1.Now()}}
	if err := st.UpdateStatus(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := st.Get(ctx, store.KeyOf(created), current); err != nil {
		t.Fatal(err)
	}
	rv2 := current.ResourceVersion
	replace(captures(rv2))
	waitFor(t, func() bool { return revision() == rv2 })
	if p.plans.Load() != 1 {
		t.Fatal("a status-only write triggered a sync")
	}
	for _, reviewed := range []string{rv1, rv2} {
		capture, err := h.CaptureCommand(ctx, id, reviewed)
		if err != nil {
			t.Fatalf("command reviewed at %s after a status-only write to %s: %v", reviewed, rv2, err)
		}
		if capture.Revision() != rv2 {
			t.Fatalf("capture runs on %s, want the current frame %s", capture.Revision(), rv2)
		}
		if _, err := capture.Check(ctx); err != nil {
			t.Fatalf("check reviewed at %s: %v", reviewed, err)
		}
	}

	// A metadata change is not status-only: both earlier reviews are stale.
	current.Labels = map[string]string{"team": "dns"}
	current.Status = public.IntegrationStatus{}
	applied, err := st.Apply(ctx, current, store.ApplyOptions{Manager: "test"})
	if err != nil {
		t.Fatal(err)
	}
	rv3 := applied.GetResourceVersion()
	replace(captures(rv3))
	waitFor(t, func() bool { return revision() == rv3 })
	for _, stale := range []string{rv1, rv2} {
		if _, err := h.CaptureCommand(ctx, id, stale); !errors.Is(err, ErrFenced) {
			t.Fatalf("command reviewed at %s before a label change was accepted: %v", stale, err)
		}
	}
	if _, err := h.CaptureCommand(ctx, id, rv3); err != nil {
		t.Fatalf("command at the labeled revision: %v", err)
	}
	if _, err := h.CaptureCommand(ctx, id, "unrelated"); !errors.Is(err, ErrFenced) {
		t.Fatal("an unknown revision was accepted")
	}
}

func TestStatusOnly(t *testing.T) {
	base := webhook("sink")
	base.ResourceVersion, base.Generation = "1", 1
	same := base.DeepCopy()
	same.ResourceVersion = "2"
	same.Status.Clients = 3
	if !StatusOnly(base, same) {
		t.Fatal("a status write was not status-only")
	}
	now := metav1.Now()
	for name, change := range map[string]func(*public.Integration){
		"generation": func(d *public.Integration) { d.Generation++ },
		"spec":       func(d *public.Integration) { d.Spec.Suspend = true },
		"uid":        func(d *public.Integration) { d.UID = "other" },
		"label":      func(d *public.Integration) { d.Labels = map[string]string{"a": "b"} },
		"annotation": func(d *public.Integration) { d.Annotations = map[string]string{"a": "b"} },
		"finalizer":  func(d *public.Integration) { d.Finalizers = []string{"x"} },
		"deletion":   func(d *public.Integration) { d.DeletionTimestamp = &now },
		"owner":      func(d *public.Integration) { d.OwnerReferences = []metav1.OwnerReference{{Name: "x"}} },
	} {
		next := same.DeepCopy()
		change(next)
		if StatusOnly(base, next) {
			t.Errorf("%s change counted as status-only", name)
		}
	}
	if StatusOnly(nil, same) || StatusOnly(base, nil) {
		t.Fatal("nil counted as status-only")
	}
}
