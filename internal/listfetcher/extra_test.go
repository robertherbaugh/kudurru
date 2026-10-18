package listfetcher

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// TestExtraLists: the catalogue's service lists are compiled next to the
// BlockLists, and a catalogue swap triggers a rebuild that Run picks up
// without any BlockList change.
func TestExtraLists(t *testing.T) {
	st := memory.New()
	objs := newStoreObjects(st)
	applyList(t, st, "mine", v1alpha1.ListSource{Name: "inline", Inline: []string{"ads.example"}})
	h := catalogue.NewHolder(nil)
	rep := newReplica(t, objs, ResolverOptions{Membership: cluster.Self{Name: "node-a"}, Extra: h, RetryInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rep.r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the first snapshot", func() bool { return rep.sink.last() != nil })
	snap := rep.sink.last()
	if snap.List("default/mine") == nil && snap.List("mine") == nil {
		t.Errorf("BlockList missing: %v", snap.Lists())
	}
	if snap.List("service:youtube") == nil || len(snap.Lists()) != 1+len(catalogue.Embedded().Services) {
		t.Fatalf("catalogue lists missing: %d lists", len(snap.Lists()))
	}
	if got := snap.Contains("www.youtube.com"); got["service:youtube"] != "||youtube.com^" {
		t.Errorf("Contains = %v", got)
	}

	next, err := catalogue.Parse([]byte(`{"schemaVersion":1,"version":"2099.01.01","license":"Apache-2.0",
		"services":[{"id":"example","name":"Example","category":"video","rules":["||video.example^"],"source":"test"}],"safeSearch":[]}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	n := rep.sink.count()
	h.Set(next)
	waitFor("the rebuild after the catalogue swap", func() bool { return rep.sink.count() > n })
	snap = rep.sink.last()
	if snap.List("service:example") == nil || snap.List("service:youtube") != nil {
		t.Errorf("after the swap: %d lists", len(snap.Lists()))
	}
}
