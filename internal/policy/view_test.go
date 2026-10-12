package policy

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestBuilderAndSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memory.New()
	defer func() { _ = st.Close() }()
	for _, o := range []store.Object{
		group(ns, "default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}),
		group(ns, "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
		pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}),
		pol(ns, "kids", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"ads"}, Priority: 10}),
		blockList(ns, "ads", "block", "||ads.example^"),
	} {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	zc, err := zonecache.New(st, zonecache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := zc.Register(Builder(Options{}, nil)); err != nil {
		t.Fatal(err)
	}
	src := Source{Cache: zc}
	if (Source{}).Index() != nil {
		t.Fatal("a Source without a cache has no index")
	}
	go func() { _ = zc.Start(ctx) }()

	kids := pipeline.Client{Addr: client("10.0.20.5").Addr}
	waitFor(t, "kids policy without lists", func() bool {
		ix := src.Index()
		if ix == nil {
			return false
		}
		d := ix.SelectPolicy(kids, time.Now())
		return d.Policy == "default/kids" && len(ix.NotLoaded()) == 1
	})

	// The list compiler publishes lists; the next snapshot resolves them.
	zc.SetLists(compile(t, map[string][]string{"default/ads": {"||ads.example^"}}, nil))
	waitFor(t, "kids policy with the ads list", func() bool {
		ix := src.Index()
		d := ix.SelectPolicy(kids, time.Now())
		return len(ix.NotLoaded()) == 0 && d.Selection.Evaluate("x.ads.example.").Blocked()
	})
	if g := src.Index().Generation(); g < 2 {
		t.Errorf("generation %d, want >= 2", g)
	}
	// A record-set-only change reuses the Index.
	before := src.Index()
	zone := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypeForward}}
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the zone in a new snapshot", func() bool {
		_, ok := zc.Snapshot().Objects.Zone("home")
		return ok
	})
	if src.Index() != before {
		t.Error("a zone change must not rebuild the policy index")
	}
	if got := (Static{Idx: src.Index()}).Index(); got != src.Index() {
		t.Error("Static returns its index")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFromRequest(t *testing.T) {
	if FromRequest(nil) != nil || FromRequest(&pipeline.Request{}) != nil {
		t.Fatal("no decision expected")
	}
	d := &Decision{Policy: "x"}
	if FromRequest(&pipeline.Request{Policy: d}) != d {
		t.Fatal("decision lost")
	}
	if FromRequest(&pipeline.Request{Policy: "not a decision"}) != nil {
		t.Fatal("foreign values are ignored")
	}
}
