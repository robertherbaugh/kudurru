package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"golang.org/x/sync/singleflight"
)

type captureLease struct {
	refs *atomic.Int64
	view *View
	once sync.Once
}

func (l *captureLease) Value() any { return l.view }
func (l *captureLease) Clone() (pipeline.ServingLease, error) {
	l.refs.Add(1)
	return &captureLease{refs: l.refs, view: l.view}, nil
}
func (l *captureLease) Release() { l.once.Do(func() { l.refs.Add(-1) }) }

func TestCapturedFetchRetainsLeaderJoinerAndFencesABA(t *testing.T) {
	h := newHarness(t, nil)
	var current atomic.Pointer[View]
	newView := func(ttl time.Duration) *View {
		t.Helper()
		var v *View
		var err error
		v, err = NewView(Settings{MaxTTL: ttl, FetchTimeout: time.Second}, false, func() bool { return current.Load() == v })
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a1, b, a2 := newView(11*time.Second), newView(22*time.Second), newView(11*time.Second)
	current.Store(a1)
	h.stage.Capture = func(r *pipeline.Request) (*View, error) { return r.Serving.Value().(*View), nil }
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{"192.0.2.53"}}}}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	resolver := upstreamstage.NewStage(set)
	resolution, err := upstreamstage.NewView(set, nil, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	resolver.Capture = func(*pipeline.Request) (*upstreamstage.View, error) { return resolution, nil }
	h.stage.ecs = resolver
	refs := &atomic.Int64{}
	refs.Store(2)
	r1, r2 := request("held.example.", dns.TypeA, 1), request("held.example.", dns.TypeA, 2)
	r1.Serving = &captureLease{refs: refs, view: a1}
	r2.Serving = &captureLease{refs: refs, view: a1}
	for _, r := range []*pipeline.Request{r1, r2} {
		if _, err = h.stage.pin(r); err != nil {
			t.Fatal(err)
		}
		if _, err = resolver.Bind(r); err != nil {
			t.Fatal(err)
		}
	}
	key1, _ := h.stage.key(r1)
	start, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	next := func(r *pipeline.Request) (*pipeline.Response, error) {
		calls.Add(1)
		close(start)
		<-finish
		if r.Serving.Value() != a1 || refs.Load() < 1 {
			return nil, errors.New("detached capture expired")
		}
		out, e := forwarded("192.0.2.1", 300)(r)
		out.CacheScope = r.Resolution.(*upstream.QueryPlan).Scope()
		return out, e
	}
	ch1 := h.stage.fetch(key1, r1, next)
	<-start
	ch2 := h.stage.fetch(key1, r2, next)
	// Neither caller waits: cancellation/stale timeout must not strand a joined clone.
	r1.Serving.Release()
	r2.Serving.Release()
	if refs.Load() != 2 {
		t.Fatalf("leader/joiner refs=%d", refs.Load())
	}
	current.Store(b)
	current.Store(a2)
	r3 := request("held.example.", dns.TypeA, 3)
	refs2 := &atomic.Int64{}
	refs2.Store(1)
	r3.Serving = &captureLease{refs: refs2, view: a2}
	if _, err = h.stage.pin(r3); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Bind(r3); err != nil {
		t.Fatal(err)
	}
	key2, _ := h.stage.key(r3)
	if key1 == key2 {
		t.Fatal("A1 scope revived by A2")
	}
	good := func(r *pipeline.Request) (*pipeline.Response, error) {
		out, e := forwarded("192.0.2.2", 300)(r)
		out.CacheScope = r.Resolution.(*upstream.QueryPlan).Scope()
		return out, e
	}
	if result := <-h.stage.fetch(key2, r3, good); result.Err != nil {
		t.Fatal(result.Err)
	}
	r3.Serving.Release()
	close(finish)
	for _, ch := range []<-chan singleflight.Result{ch1, ch2} {
		if result := <-ch; result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	h.stage.Wait()
	if refs.Load() != 0 || refs2.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("ownership refs=%d,%d calls=%d", refs.Load(), refs2.Load(), calls.Load())
	}
	if _, ok := h.stage.c.Peek(key1); ok {
		t.Fatal("retired A1 completed into cache")
	}
	entry, ok := h.stage.c.Peek(key2)
	if !ok || entry.TTL != 11*time.Second {
		t.Fatalf("A2 entry missing or wrong pinned TTL: %+v", entry)
	}
	msg := new(dns.Msg)
	if err = msg.Unpack(entry.Wire); err != nil {
		t.Fatal(err)
	}
	if msg.Answer[1].(*dns.A).A.String() != "192.0.2.2" {
		t.Fatal("old completion changed A2")
	}
}

func TestCapturePinsLegacyFetchSettingsThroughReload(t *testing.T) {
	h := newHarness(t, nil)
	h.stage.SetSettings(Settings{MaxTTL: 7 * time.Second, FetchTimeout: time.Second})
	start, finish := make(chan struct{}), make(chan struct{})
	h.up.set(func(r *pipeline.Request) (*pipeline.Response, error) {
		close(start)
		<-finish
		return forwarded("192.0.2.7", 300)(r)
	})
	r := request("settings.example.", dns.TypeA, 1)
	ctx, cancel := context.WithCancel(context.Background())
	r.Ctx = ctx
	done := make(chan error, 1)
	go func() { _, e := h.stage.Serve(r, h.up.next); done <- e }()
	<-start
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	h.stage.SetSettings(Settings{MaxTTL: 99 * time.Second, FetchTimeout: time.Nanosecond})
	close(finish)
	h.stage.Wait()
	k, _ := h.stage.key(r)
	entry, ok := h.stage.c.Peek(k)
	if !ok || entry.TTL != 7*time.Second {
		t.Fatalf("detached settings changed: %+v", entry)
	}
}

func TestCacheCaptureMissingFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	refs := &atomic.Int64{}
	refs.Store(1)
	r := request("fail.example.", dns.TypeA, 1)
	r.Serving = &captureLease{refs: refs}
	if _, err := h.stage.Serve(r, h.up.next); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal(err)
	}
	if h.up.calls.Load() != 0 {
		t.Fatal("missing capture fell through")
	}
	r.Serving.Release()
}

func TestCapturedStaleAndPrefetchOutliveCaller(t *testing.T) {
	for _, mode := range []string{"stale", "prefetch"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, nil)
			view, err := NewView(Settings{MaxTTL: time.Second, StaleMaxWait: 5 * time.Millisecond, FetchTimeout: time.Second}, false, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			h.stage.Capture = func(r *pipeline.Request) (*View, error) { return r.Serving.Value().(*View), nil }
			set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{"192.0.2.53"}}}}, upstream.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = set.Close() }()
			resolver := upstreamstage.NewStage(set)
			rv, err := upstreamstage.NewView(set, nil, false, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			resolver.Capture = func(*pipeline.Request) (*upstreamstage.View, error) { return rv, nil }
			h.stage.ecs = resolver
			refs := &atomic.Int64{}
			refs.Store(1)
			r := request("late.example.", dns.TypeA, 1)
			r.Serving = &captureLease{refs: refs, view: view}
			if _, err = h.stage.pin(r); err != nil {
				t.Fatal(err)
			}
			p, err := resolver.Bind(r)
			if err != nil {
				t.Fatal(err)
			}
			k, _ := h.stage.key(r)
			initial, err := forwarded("192.0.2.1", 300)(r)
			if err != nil {
				t.Fatal(err)
			}
			initial.CacheScope = view.scope(p.Scope())
			h.stage.storeWithSettings(k, initial, h.stage.c.Generation(), &view.settings)
			entered, finish := make(chan struct{}), make(chan struct{})
			next := func(fr *pipeline.Request) (*pipeline.Response, error) {
				close(entered)
				<-finish
				if refs.Load() != 1 || fr.Serving.Value() != view {
					return nil, errors.New("late work lost lease")
				}
				out, e := forwarded("192.0.2.2", 300)(fr)
				out.CacheScope = fr.Resolution.(*upstream.QueryPlan).Scope()
				return out, e
			}
			if mode == "stale" {
				h.clk.Advance(2 * time.Second)
				out, e := h.stage.Serve(r, next)
				if e != nil || out.Status != pipeline.StatusCachedStale {
					t.Fatal("stale did not return before fetch", e)
				}
			} else {
				h.stage.prefetch(k, r, next)
			}
			<-entered
			r.Serving.Release()
			if refs.Load() != 1 {
				t.Fatalf("late capture refs %d", refs.Load())
			}
			close(finish)
			h.stage.Wait()
			if refs.Load() != 0 {
				t.Fatal("late capture leaked")
			}
		})
	}
}
