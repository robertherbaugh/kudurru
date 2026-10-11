package listfetcher

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestEnabledBlockLists(t *testing.T) {
	mk := func(name, enabled string) *v1alpha1.BlockList {
		bl := &v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if enabled != "" {
			bl.Annotations = map[string]string{v1alpha1.EnabledAnnotation: enabled}
		}
		return bl
	}
	all := []*v1alpha1.BlockList{mk("a", ""), mk("b", "false"), mk("c", "true"), mk("d", "False")}
	got := EnabledBlockLists(all)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("EnabledBlockLists = %v", got)
	}
	if len(all) != 4 || all[1].Name != "b" {
		t.Fatal("the input must not be modified")
	}
	none := all[:1]
	if got := EnabledBlockLists(none); &got[0] != &none[0] {
		t.Fatal("no disabled list: the input is returned as is")
	}
}

// TestDisabledListUnloads: kudurru.io/enabled: "false" unloads a list on
// every node as if it were deleted; removing the annotation loads it again.
func TestDisabledListUnloads(t *testing.T) {
	st := memory.New()
	zc, err := zonecache.New(st, zonecache.Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(ResolverOptions{Objects: NewZoneCacheObjects(zc), Cache: cache, Sink: zc, Logger: quiet(), InitStrategy: InitBlocking})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- zc.Start(ctx) }()
	go func() { errs <- r.Run(ctx) }()

	apply := func(enabled string) {
		t.Helper()
		bl := &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: "kids", Namespace: "home", Annotations: map[string]string{v1alpha1.EnabledAnnotation: enabled}},
			Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"||games.example^"}}}},
		}
		if _, err := st.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(what string, cond func(*zonecache.Snapshot) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond(zc.Snapshot()) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	loaded := func(s *zonecache.Snapshot) bool {
		l := s.Objects.Lists()
		return l != nil && l.List("home/kids") != nil
	}
	apply("true")
	waitFor("the list", loaded)
	apply("false")
	waitFor("the unload", func(s *zonecache.Snapshot) bool { return !loaded(s) })
	if ok, msgs := r.Ready(); !ok {
		t.Errorf("a disabled list must not hold readiness: %v", msgs)
	}
	apply("true")
	waitFor("the reload", loaded)
	cancel()
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("runnable: %v", err)
		}
	}
}
