package cluster

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// fakeElector grants and revokes terms on demand and enforces the
// store.Elector contract: terms never overlap and onStopped runs after
// onStarted returned.
type fakeElector struct {
	grant  chan struct{}
	revoke chan struct{}

	leading atomic.Bool
	running atomic.Bool
	terms   atomic.Int32
	runErr  error
}

var _ store.Elector = (*fakeElector)(nil)

func newFakeElector() *fakeElector {
	return &fakeElector{grant: make(chan struct{}), revoke: make(chan struct{})}
}

func (f *fakeElector) Run(ctx context.Context, onStarted func(context.Context), onStopped func()) error {
	if f.runErr != nil {
		return f.runErr
	}
	if !f.running.CompareAndSwap(false, true) {
		return errors.New("fake: already running")
	}
	defer f.running.Store(false)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.grant:
		}
		f.terms.Add(1)
		termCtx, cancel := context.WithCancel(ctx)
		f.leading.Store(true)
		started := make(chan struct{})
		go func() {
			defer close(started)
			onStarted(termCtx)
		}()
		select {
		case <-f.revoke:
		case <-ctx.Done():
		}
		cancel()
		<-started
		f.leading.Store(false)
		onStopped()
	}
}

func (f *fakeElector) IsLeader() bool { return f.leading.Load() }
func (f *fakeElector) Leader() string { return "fake" }

// send delivers a grant or revoke, failing the test if Run is not
// listening.
func send(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case ch <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatalf("elector did not accept %s", what)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder is a concurrency-safe ordered log of lifecycle events.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) count(s string) int {
	n := 0
	for _, e := range r.list() {
		if e == s {
			n++
		}
	}
	return n
}

// blocker is a runnable that records start and stop and blocks until its
// context ends.
func blocker(rec *recorder, name string) RunFunc {
	return func(ctx context.Context) error {
		rec.add(name + ":start")
		<-ctx.Done()
		rec.add(name + ":stop")
		return nil
	}
}

type harness struct {
	r      *Runner
	e      *fakeElector
	rec    *recorder
	reg    *prometheus.Registry
	cancel context.CancelFunc
	done   chan error
}

func startHarness(t *testing.T, opts Options, add func(h *harness)) *harness {
	t.Helper()
	h := &harness{e: newFakeElector(), rec: &recorder{}, reg: prometheus.NewRegistry(), done: make(chan error, 1)}
	if opts.Elector == nil {
		opts.Elector = h.e
	}
	opts.Registerer = h.reg
	opts.Logger = quietLogger()
	if opts.RestartBackoff == 0 {
		opts.RestartBackoff = 5 * time.Millisecond
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	add(h)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})
	return h
}

func (h *harness) stop(t *testing.T) error {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func gauge(t *testing.T, h *harness, name, role string) float64 {
	t.Helper()
	var g *prometheus.GaugeVec
	switch name {
	case "kudurru_leader":
		g = h.r.metrics.leader
	case "kudurru_role_active":
		g = h.r.metrics.active
	}
	return testutil.ToFloat64(g.WithLabelValues(role))
}

// TestLeadershipHandoff: always-on work starts at once; leader-only work
// starts with a term, stops when it ends, restarts with the next term, and
// the Runner keeps running throughout.
func TestLeadershipHandoff(t *testing.T) {
	h := startHarness(t, Options{}, func(h *harness) {
		mustAdd(t, h.r, "dns", false, blocker(h.rec, "dns"))
		mustAdd(t, h.r, "controller", true, blocker(h.rec, "controller"))
		mustAdd(t, h.r, "listfetcher", true, blocker(h.rec, "listfetcher"))
	})
	eventually(t, "dns start", func() bool { return h.rec.count("dns:start") == 1 })
	time.Sleep(20 * time.Millisecond)
	if h.rec.count("controller:start") != 0 || h.r.IsLeader() {
		t.Fatal("leader-only runnable started without a term")
	}
	if gauge(t, h, "kudurru_leader", "controller") != 0 || gauge(t, h, "kudurru_role_active", "dns") != 1 {
		t.Fatal("gauges before the first term")
	}

	for term := 1; term <= 3; term++ {
		send(t, h.e.grant, "grant")
		eventually(t, "leader group start", func() bool {
			return h.rec.count("controller:start") == term && h.rec.count("listfetcher:start") == term
		})
		if !h.r.IsLeader() || gauge(t, h, "kudurru_leader", "controller") != 1 || gauge(t, h, "kudurru_role_active", "controller") != 1 {
			t.Fatalf("term %d: leader state not reported", term)
		}
		send(t, h.e.revoke, "revoke")
		eventually(t, "leader group stop", func() bool {
			return h.rec.count("controller:stop") == term && h.rec.count("listfetcher:stop") == term && !h.r.IsLeader()
		})
		if gauge(t, h, "kudurru_leader", "controller") != 0 || gauge(t, h, "kudurru_role_active", "controller") != 0 {
			t.Fatalf("term %d: gauges after loss", term)
		}
		select {
		case err := <-h.done:
			t.Fatalf("Run returned on leadership loss: %v", err)
		default:
		}
		if h.rec.count("dns:stop") != 0 {
			t.Fatal("always-on runnable stopped on leadership loss")
		}
	}
	if got := h.r.Terms(); got != 3 {
		t.Fatalf("Terms() = %d", got)
	}
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

// TestShutdownOrder: lameduck, then the leader group, then always-on.
func TestShutdownOrder(t *testing.T) {
	var h *harness
	h = startHarness(t, Options{
		Lameduck:         func(context.Context) { h.rec.add("lameduck") },
		LameduckDuration: 10 * time.Millisecond,
	}, func(h *harness) {
		mustAdd(t, h.r, "api", false, blocker(h.rec, "api"))
		mustAdd(t, h.r, "controller", true, blocker(h.rec, "controller"))
	})
	send(t, h.e.grant, "grant")
	eventually(t, "both running", func() bool { return h.rec.count("controller:start") == 1 && h.rec.count("api:start") == 1 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(h.rec.list()[2:], ",")
	if got != "lameduck,controller:stop,api:stop" {
		t.Fatalf("shutdown order %s", got)
	}
}

// TestLeaderOnlyFailureRestarts: a failing or panicking leader-only
// runnable is restarted within the term and never stops the Runner.
func TestLeaderOnlyFailureRestarts(t *testing.T) {
	var calls atomic.Int32
	h := startHarness(t, Options{}, func(h *harness) {
		mustAdd(t, h.r, "flaky", true, func(ctx context.Context) error {
			switch calls.Add(1) {
			case 1:
				return errors.New("boom")
			case 2:
				panic("bug")
			}
			<-ctx.Done()
			return nil
		})
	})
	send(t, h.e.grant, "grant")
	eventually(t, "third attempt", func() bool { return calls.Load() == 3 })
	st := h.r.Status()
	if len(st) != 1 || st[0].Restarts != 2 || !strings.Contains(st[0].LastError, "panic: bug") {
		t.Fatalf("status %+v", st)
	}
	eventually(t, "active", func() bool { return h.r.Status()[0].Active })
	select {
	case err := <-h.done:
		t.Fatalf("Run returned: %v", err)
	default:
	}
}

// TestAlwaysOnFailureIsFatal: an always-on runnable failing ends Run with
// its error after the ordered shutdown.
func TestAlwaysOnFailureIsFatal(t *testing.T) {
	h := startHarness(t, Options{}, func(h *harness) {
		mustAdd(t, h.r, "controller", true, blocker(h.rec, "controller"))
		mustAdd(t, h.r, "dns", false, func(context.Context) error { return errors.New("bind: address in use") })
	})
	send(t, h.e.grant, "grant")
	select {
	case err := <-h.done:
		h.done <- err
		if err == nil || !strings.Contains(err.Error(), "dns: bind") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if h.rec.count("controller:start") == 1 && h.rec.count("controller:stop") != 1 {
		t.Fatal("leader group not stopped on fatal shutdown")
	}
}

// TestNoLeaderOnlyNeverCampaigns: without leader-only runnables the
// elector is never run.
func TestNoLeaderOnlyNeverCampaigns(t *testing.T) {
	h := startHarness(t, Options{}, func(h *harness) {
		mustAdd(t, h.r, "dns", false, blocker(h.rec, "dns"))
	})
	eventually(t, "dns start", func() bool { return h.rec.count("dns:start") == 1 })
	time.Sleep(20 * time.Millisecond)
	if h.e.running.Load() {
		t.Fatal("elector running without leader-only runnables")
	}
}

// TestElectorErrorIsFatal: an elector that cannot run (invalid lease
// name) fails Run instead of silently never leading.
func TestElectorErrorIsFatal(t *testing.T) {
	e := newFakeElector()
	e.runErr = errors.New("invalid lease name")
	h := startHarness(t, Options{Elector: e}, func(h *harness) {
		mustAdd(t, h.r, "controller", true, blocker(h.rec, "controller"))
	})
	select {
	case err := <-h.done:
		h.done <- err
		if err == nil || !strings.Contains(err.Error(), "invalid lease name") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestMemoryStoreElector: the memory driver's always-leader elector starts
// the leader group at once and stops it on shutdown.
func TestMemoryStoreElector(t *testing.T) {
	st := memory.New()
	h := startHarness(t, Options{Elector: st.Elector(ElectionController)}, func(h *harness) {
		mustAdd(t, h.r, "controller", true, blocker(h.rec, "controller"))
	})
	eventually(t, "controller start", func() bool { return h.rec.count("controller:start") == 1 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if h.rec.count("controller:stop") != 1 {
		t.Fatal("leader group not stopped")
	}
}

func TestAddValidation(t *testing.T) {
	r, err := New(Options{Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	noop := func(context.Context) error { return nil }
	tests := []struct {
		name       string
		runnable   string
		leaderOnly bool
		fn         RunFunc
		want       string
	}{
		{"empty name", "", false, noop, "needs a name"},
		{"nil fn", "x", false, nil, "needs a name"},
		{"leader-only without elector", "controller", true, noop, "needs Options.Elector"},
		{"ok", "dns", false, noop, ""},
		{"duplicate", "dns", false, noop, "added twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.Add(tt.runnable, tt.leaderOnly, tt.fn)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("late", false, noop); err == nil {
		t.Fatal("Add after Run must fail")
	}
	if err := r.Run(ctx); err == nil {
		t.Fatal("second Run must fail")
	}
}

func mustAdd(t *testing.T, r *Runner, name string, leaderOnly bool, fn RunFunc) {
	t.Helper()
	if err := r.Add(name, leaderOnly, fn); err != nil {
		t.Fatal(err)
	}
}
