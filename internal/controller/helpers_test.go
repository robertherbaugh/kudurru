package controller

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// countingStore wraps a Store, counts UpdateStatus calls per kind, can
// fail them on demand, and can hide live watch events (to prove that the
// resync repairs a missed event).
type countingStore struct {
	store.Store
	mu      sync.Mutex
	writes  map[store.Kind]int
	failN   atomic.Int32 // fail the next N UpdateStatus calls
	failErr error
	hide    atomic.Bool // drop live (post-bookmark) watch events
}

func newCountingStoreOver(inner store.Store) *countingStore {
	return &countingStore{Store: inner, writes: map[store.Kind]int{}}
}

func (s *countingStore) UpdateStatus(ctx context.Context, obj store.Object) error {
	if s.failN.Load() > 0 {
		s.failN.Add(-1)
		return s.failErr
	}
	if err := s.Store.UpdateStatus(ctx, obj); err != nil {
		return err
	}
	s.mu.Lock()
	s.writes[store.KindOf(obj)]++
	s.mu.Unlock()
	return nil
}

// Writes returns the UpdateStatus count of a kind ("" = all kinds).
func (s *countingStore) Writes(kind store.Kind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != "" {
		return s.writes[kind]
	}
	n := 0
	for _, v := range s.writes {
		n += v
	}
	return n
}

func (s *countingStore) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (store.Watcher, error) {
	w, err := s.Store.Watch(ctx, kind, opts)
	if err != nil {
		return nil, err
	}
	out := &filterWatcher{inner: w, ch: make(chan store.Event, 1024)}
	go func() {
		defer close(out.ch)
		live := false
		for ev := range w.ResultChan() {
			if ev.Type == store.Bookmark {
				live = true
			} else if live && s.hide.Load() {
				continue
			}
			out.ch <- ev
		}
	}()
	return out, nil
}

type filterWatcher struct {
	inner store.Watcher
	ch    chan store.Event
}

func (w *filterWatcher) ResultChan() <-chan store.Event { return w.ch }
func (w *filterWatcher) Stop()                          { w.inner.Stop() }

// fakeClock is an adjustable clock.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.ns.Store(t.UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// t0 is the fixed start of the fake clock: 2026-10-11T00:00:00Z.
var t0 = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)

// unix32 is t as a serial; test times fit in 32 bits.
func unix32(t time.Time) uint32 { return uint32(t.Unix()) } //nolint:gosec // test times are far from the 2106 limit

type env struct {
	t      *testing.T
	st     *countingStore
	ctrl   *Controller
	events *MemorySink
	clock  *fakeClock
	cancel context.CancelFunc
	done   chan error
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fastLimiter retries quickly so failure tests finish fast.
func fastLimiter() workqueue.TypedRateLimiter[store.Key] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[store.Key](time.Millisecond, 50*time.Millisecond)
}

// newEnv builds a controller over a fresh counting memory store. Call
// start to run it.
func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	return newEnvOver(t, memory.New(), mutate...)
}

// newEnvOver builds a controller over a counting wrapper of inner.
func newEnvOver(t *testing.T, inner store.Store, mutate ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, st: newCountingStoreOver(inner), events: &MemorySink{}, clock: newFakeClock(t0)}
	// Tests see every intermediate zone state unless they opt into
	// coalescing.
	opts := Options{Events: e.events, Now: e.clock.Now, Logger: quiet(), RateLimiter: fastLimiter, ZoneCoalesce: -1}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(e.st, opts)
	if err != nil {
		t.Fatal(err)
	}
	e.ctrl = c
	t.Cleanup(func() { e.stop() })
	return e
}

func (e *env) start() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- e.ctrl.Run(ctx) }()
	e.eventually("caches synced", e.ctrl.Synced)
}

func (e *env) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case err := <-e.done:
		if err != nil {
			e.t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		e.t.Error("controller did not stop")
	}
	e.cancel = nil
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settle waits until the store has seen no status write for a while, so
// write counts can be compared.
func (e *env) settle() {
	e.t.Helper()
	last, stable := -1, 0
	e.eventually("status writes to settle", func() bool {
		n := e.st.Writes("")
		if n == last {
			stable++
		} else {
			last, stable = n, 0
		}
		return stable >= 25
	})
}

func (e *env) apply(obj store.Object) {
	e.t.Helper()
	if _, err := e.st.Apply(context.Background(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) del(obj store.Object) {
	e.t.Helper()
	if err := e.st.Delete(context.Background(), store.KeyOf(obj), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) zone(name string) *v1alpha1.DNSZone {
	e.t.Helper()
	var z v1alpha1.DNSZone
	if err := e.st.Get(context.Background(), store.Key{Kind: KindDNSZone, Name: name}, &z); err != nil {
		e.t.Fatal(err)
	}
	return &z
}

func (e *env) rrset(ns, name string) *v1alpha1.DNSRecordSet {
	e.t.Helper()
	var rs v1alpha1.DNSRecordSet
	if err := e.st.Get(context.Background(), store.Key{Kind: KindDNSRecordSet, Namespace: ns, Name: name}, &rs); err != nil {
		e.t.Fatal(err)
	}
	return &rs
}

func (e *env) policy(ns, name string) *v1alpha1.Policy {
	e.t.Helper()
	var p v1alpha1.Policy
	if err := e.st.Get(context.Background(), store.Key{Kind: KindPolicy, Namespace: ns, Name: name}, &p); err != nil {
		e.t.Fatal(err)
	}
	return &p
}

func (e *env) eventCount(reason string, key store.Key) int {
	n := 0
	for _, ev := range e.events.Events() {
		if ev.Reason == reason && ev.Key == key {
			n++
		}
	}
	return n
}

// cond returns "Status/Reason" of a condition, or "" when absent.
func cond(conds []metav1.Condition, t string) string {
	c := condition(conds, t)
	if c == nil {
		return ""
	}
	return string(c.Status) + "/" + c.Reason
}

func zoneObj(name, zoneName string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: zoneName, Type: v1alpha1.ZoneTypePrimary, DefaultTTL: 300},
	}
}

// rrsetObj builds a record set created at t0+age.
func rrsetObj(ns, name, zone string, age time.Duration, eps ...v1alpha1.Endpoint) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(t0.Add(age))},
		Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: zone}, Endpoints: eps},
	}
}

func ep(name, typ string, targets ...string) v1alpha1.Endpoint {
	return v1alpha1.Endpoint{DNSName: name, RecordType: typ, Targets: targets}
}

func key(kind store.Kind, ns, name string) store.Key {
	return store.Key{Kind: kind, Namespace: ns, Name: name}
}

// sumCounter adds up every series of a counter vector.
func sumCounter(t *testing.T, c *prometheus.CounterVec) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() { c.Collect(ch); close(ch) }()
	total := 0.0
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatal(err)
		}
		total += pb.GetCounter().GetValue()
	}
	return total
}
