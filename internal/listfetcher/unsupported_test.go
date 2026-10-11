package listfetcher

import (
	"context"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

// TestUnsupportedAndSubdomains: status.unsupported counts the rules this
// version parses but does not apply ($important, $badfilter), and
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
	ctx := context.Background()
	fx.f.Reconcile(ctx)
	if got := getList(t, fx.st, "ads").Status.Unsupported; got != 2 {
		t.Errorf("status.unsupported = %d, want 2", got)
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
	for name, want := range map[string]bool{
		"sub.example": true, "www.sub.example": true,
		"inline-sub.example": true, "www.inline-sub.example": true,
		"exact.example": true, "www.exact.example": false,
		"c.example": true,
	} {
		if got := snap.Contains(name)["default/ads"] != ""; got != want {
			t.Errorf("%s blocked = %v, want %v", name, got, want)
		}
	}
}
