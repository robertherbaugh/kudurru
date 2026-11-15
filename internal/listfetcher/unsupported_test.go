package listfetcher

import (
	"context"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

// TestUnsupportedAndSubdomains: supported modifiers do not contribute to
// status.unsupported or unsupported diagnostics, and
// spec.sources[].subdomains makes plain-domain entries match their
// subdomains on the replicas, for downloaded and inline sources alike.
func TestUnsupportedAndSubdomains(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, "sub.example\n")
	applyList(t, fx.st, "ads",
		v1alpha1.ListSource{URL: srv.URL + "/domains", Format: "domains", Subdomains: true},
		v1alpha1.ListSource{Inline: []string{"||a.example^$important", "||b.example^$badfilter", "||c.example^"}},
		v1alpha1.ListSource{Inline: []string{"exact.example"}, Format: "domains"},
		v1alpha1.ListSource{Inline: []string{"inline-sub.example"}, Format: "domains", Subdomains: true},
	)
	applyList(t, fx.st, "diagnostics", v1alpha1.ListSource{Inline: []string{
		"||geo.example^$respgeo=US", // Known, but not implemented.
		"||unknown.example^$future", // Unknown: reject the whole rule.
		"||ordinary.example^",
	}})
	ctx := context.Background()
	fx.f.Reconcile(ctx)
	if got := getList(t, fx.st, "ads").Status.Unsupported; got != 0 {
		t.Errorf("status.unsupported = %d, want 0", got)
	}
	status := getList(t, fx.st, "diagnostics").Status
	if status.Unsupported != 1 || status.Invalid != 1 {
		t.Errorf("diagnostic counts unsupported=%d invalid=%d, want 1 each", status.Unsupported, status.Invalid)
	}

	addr, _ := startInternal(t, fx.cache, nil)
	rep := newReplica(t, newStoreObjects(fx.st), ResolverOptions{
		Membership: &cluster.Static{SelfName: "node-b", LeaderName: "node-a", Members: []cluster.Peer{{Name: "node-a", Address: addr}}},
		Now:        fx.clk.Now,
	})
	if _, err := rep.r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	snap := rep.sink.last()
	if snap == nil {
		t.Fatal("no compiled list handed to the sink")
	}
	list := snap.List("default/ads")
	if list == nil {
		t.Fatal("no compiled ads list")
	}
	if got := list.Stats().Unsupported; got != 0 {
		t.Errorf("replica unsupported = %d, want 0", got)
	}
	if got := list.Unsupported(); len(got) != 0 {
		t.Errorf("replica unsupported diagnostics = %v, want none", got)
	}
	if got := list.Warnings(); len(got) != 0 {
		t.Errorf("supported modifiers produced warnings: %v", got)
	}
	diagnostics := snap.List("default/diagnostics")
	if diagnostics == nil {
		t.Fatal("no compiled diagnostics list")
	}
	if got := diagnostics.Stats(); got.Unsupported != 1 || got.Invalid != 1 {
		t.Errorf("replica diagnostic counts = %+v, want unsupported=1 invalid=1", got)
	}
	if got := diagnostics.Unsupported(); len(got) != 1 || got[0].Text != "||geo.example^$respgeo=US" {
		t.Errorf("replica unsupported diagnostics = %v, want only respgeo rule", got)
	}
	if got := diagnostics.Warnings(); len(got) != 1 {
		t.Errorf("replica unsupported warnings = %v, want one", got)
	}
	for name, want := range map[string]bool{"geo.example": false, "unknown.example": false, "ordinary.example": true} {
		if got := snap.Contains(name)["default/diagnostics"] != ""; got != want {
			t.Errorf("%s blocked by diagnostics list = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{
		"sub.example": true, "www.sub.example": true,
		"inline-sub.example": true, "www.inline-sub.example": true,
		"exact.example": true, "www.exact.example": false,
		"a.example": true, "b.example": false, "c.example": true,
	} {
		if got := snap.Contains(name)["default/ads"] != ""; got != want {
			t.Errorf("%s blocked = %v, want %v", name, got, want)
		}
	}
}

func TestPreviouslyUnsupportedCountsRecompiledAfterRestart(t *testing.T) {
	fx := newFixture(t)
	applyList(t, fx.st, "ads", v1alpha1.ListSource{Inline: []string{"||important.example^$important", "||ordinary.example^"}})
	ctx := context.Background()
	fx.f.Reconcile(ctx)

	// Simulate status persisted by the previous compiler with the same source
	// digest. A new fetcher must not reuse these obsolete counts after upgrade.
	old := getList(t, fx.st, "ads")
	old.Status.Entries, old.Status.RuleCount, old.Status.Unsupported = 1, 1, 1
	old.Status.Sources[0].Entries = 1
	if err := fx.st.Store.UpdateStatus(ctx, old); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(Options{Store: fx.st, Objects: newStoreObjects(fx.st), Cache: fx.cache, Downloader: fx.dl, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	fresh.Reconcile(ctx)
	status := getList(t, fx.st, "ads").Status
	if status.Unsupported != 0 || status.RuleCount != 2 || status.Entries != 2 || status.Sources[0].Entries != 2 {
		t.Fatalf("obsolete modifier counts retained after restart: %+v", status)
	}
	writes := fx.st.updates.Load()
	fresh.Reconcile(ctx)
	if got := fx.st.updates.Load(); got != writes {
		t.Errorf("unchanged reconcile rewrote counts: writes=%d, want %d", got, writes)
	}
}
