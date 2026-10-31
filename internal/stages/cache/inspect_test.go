package cache

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestInspectRetainedScopeFlagsExpiryAndNoEffects(t *testing.T) {
	h := newHarness(t, nil)
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{"192.0.2.53"}}}}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	u := upstreamstage.NewStage(set)
	resolution, err := upstreamstage.NewView(set, nil, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	u.Capture = func(*pipeline.Request) (*upstreamstage.View, error) { return resolution, nil }
	h.stage.ecs = u
	a, _ := NewView(Settings{}, false, func() bool { return false })
	b, _ := NewView(Settings{}, false, func() bool { return true })
	h.stage.Capture = func(r *pipeline.Request) (*View, error) { return r.Serving.Value().(*View), nil }
	makeRequest := func(v *View) *pipeline.Request {
		r := request("inspect.example.", dns.TypeA, 1)
		refs := &atomic.Int64{}
		refs.Store(1)
		r.Serving = &captureLease{refs: refs, view: v}
		t.Cleanup(r.Serving.Release)
		// Plan from the real stage; never a guessed encoded scope.
		if _, err := u.Bind(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	old := makeRequest(a)
	if _, found, eligible, err := h.stage.Inspect(old); err != nil || found || !eligible {
		t.Fatal(found, eligible, err)
	}
	count := 0
	a.scopes.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatal("inspection created a cache scope")
	}
	key, _ := h.stage.key(old)
	parts, _ := key.Parts()
	h.stage.c.SetWithMetadata(key, []byte("private wire"), cachedata.Positive, time.Minute, cachedata.Metadata{Scope: parts.Scope})
	for range 10 {
		value, found, eligible, err := h.stage.Inspect(makeRequest(a))
		if err != nil || !found || !eligible || value.TTL != time.Minute || value.State != cachedata.Fresh {
			t.Fatal(value, found, eligible, err)
		}
	}
	if testutil.ToFloat64(h.m.Hits.WithLabelValues("positive", "false")) != 0 || h.up.calls.Load() != 0 {
		t.Fatal("inspection counted a hit or fetched")
	}
	if sh := h.stage.c; sh.Len() != 1 {
		t.Fatal("inspection mutated storage")
	}
	// A new byte-identical settings view cannot adopt the previous incarnation.
	if _, found, _, err := h.stage.Inspect(makeRequest(b)); err != nil || found {
		t.Fatal("new incarnation exposed retired data", err)
	}
	for _, flag := range []string{"do", "cd"} {
		r := makeRequest(a)
		if flag == "do" {
			r.Msg.SetEdns0(1232, true)
		} else {
			r.Msg.CheckingDisabled = true
			r.Resolution = nil
			if _, err := u.Bind(r); err != nil {
				t.Fatal(err)
			}
		}
		if _, found, _, err := h.stage.Inspect(r); err != nil || found {
			t.Fatal("different key exposed entry", flag, err)
		}
	}
	// Stored subnet data is not a match for an unscoped client key.
	ecsKey := cachedata.MakeScopedKey(parts.Scope, "other.example.", dns.TypeA, dns.ClassINET, false, false, netip.MustParsePrefix("198.51.100.0/24"))
	h.stage.c.SetWithMetadata(ecsKey, []byte("other client"), cachedata.Positive, time.Minute, cachedata.Metadata{Scope: parts.Scope})
	r := makeRequest(a)
	r.Msg.Question[0].Name = "other.example."
	if _, found, _, err := h.stage.Inspect(r); err != nil || found {
		t.Fatal("inspection crossed ECS partition", err)
	}
	h.clk.Advance(time.Minute)
	if value, found, _, err := h.stage.Inspect(makeRequest(a)); err != nil || !found || value.State != cachedata.Stale {
		t.Fatal("stale metadata unavailable", value, found, err)
	}
	h.clk.Advance(73 * time.Hour)
	if _, found, _, err := h.stage.Inspect(makeRequest(a)); err != nil || found {
		t.Fatal("expired data exposed", err)
	}
	canceled := makeRequest(a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled.Ctx = ctx
	if _, _, _, err := h.stage.Inspect(canceled); err == nil {
		t.Fatal("canceled read accepted")
	}
}
