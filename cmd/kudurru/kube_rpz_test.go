package main

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// D390: the Kubernetes selector rebinds only when the configuration snapshot,
// feed snapshot or ring changes, and a selected feed with no acquired copy is
// required and unavailable rather than omitted.
func TestKubeRPZBindingReuseAndFailClosed(t *testing.T) {
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "feed"}, Spec: public.BlockListSpec{Mode: "block",
		Sources: []public.ListSource{{Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{"127.0.0.1:5300"}}}}}}
	applied, err := st.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = cache.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for cache.Snapshot() == nil || !cache.Snapshot().Authoritative() {
		if time.Now().After(deadline) {
			t.Fatal("configuration snapshot never became authoritative")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ring, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	var current *dnsserver.StaticKeyring
	k, err := newKubeRPZ(cache, nil, func() *dnsserver.StaticKeyring { return current })
	if err != nil {
		t.Fatal(err)
	}
	first := k.binding()
	if first.keys != k.unsigned || !first.available || k.binding() != first {
		t.Fatal("unchanged inputs did not reuse one binding with the unsigned ring")
	}
	current = ring
	second := k.binding()
	if second == first || second.keys != ring || k.binding() != second {
		t.Fatal("a ring replacement did not rebind exactly once")
	}
	selected := second.bound.Select([]*public.BlockList{applied.(*public.BlockList)}, second.available)
	if selected.Empty() {
		t.Fatal("a selected feed without data was omitted")
	}
	if _, available := selected.Summary(time.Now()); available {
		t.Fatal("a selected feed without data was available")
	}
	if hasRPZSource(nil) || hasRPZSource(&public.BlockList{Spec: public.BlockListSpec{Sources: []public.ListSource{{URL: "https://lists.invalid/a.txt"}}}}) || !hasRPZSource(list) {
		t.Fatal("RPZ source prefilter is wrong")
	}
}
