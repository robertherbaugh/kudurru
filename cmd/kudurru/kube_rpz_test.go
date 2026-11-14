package main

import (
	"context"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
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
	// A stale local.db snapshot keeps the feed required but unavailable, so a
	// selected feed fails closed even with data (rpz Select(..., false)).
	live := cache.Snapshot()
	staleCopy := *live
	staleCopy.State, staleCopy.Stale = zonecache.FromSnapshot, true
	stale, err := newKubeRPZ(kubeRPZFixedConfig{&staleCopy}, nil, func() *dnsserver.StaticKeyring { return ring })
	if err != nil {
		t.Fatal(err)
	}
	if b := stale.binding(); b.available || b.config != &staleCopy {
		t.Fatal("a stale configuration snapshot left the feed available")
	}
	if b := k.binding(); !b.available || b.config != live {
		t.Fatal("a live configuration snapshot made the feed unavailable")
	}
	// A referenced RPZ list the resolver has not compiled yet is still
	// selected: the policy decision carries it as pending, so the feed is
	// required from the first query instead of skipped for the load window.
	ix := policy.Build(policy.Input{
		ClientGroups: []*public.ClientGroup{{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "all"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{CIDR: "0.0.0.0/0"}}}}},
		Policies:     []*public.Policy{{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"all"}, BlockListRefs: []string{"feed"}}}},
		BlockLists:   []*public.BlockList{applied.(*public.BlockList)},
	}, policy.Options{DefaultNamespace: "default"})
	if nl := ix.NotLoaded(); len(nl) != 1 {
		t.Fatalf("the list is compiled; the case proves nothing: %v", nl)
	}
	req := &pipeline.Request{Policy: ix.SelectPolicy(pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53000")}, time.Now())}
	pending, err := k.capture(req)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Empty() {
		t.Fatal("a referenced RPZ list that is not compiled yet was not selected")
	}
	if _, available := pending.Summary(time.Now()); available {
		t.Fatal("a pending list's feed without data was available")
	}
	if hasRPZSource(nil) || hasRPZSource(&public.BlockList{Spec: public.BlockListSpec{Sources: []public.ListSource{{URL: "https://lists.invalid/a.txt"}}}}) || !hasRPZSource(list) {
		t.Fatal("RPZ source prefilter is wrong")
	}
}

type kubeRPZFixedConfig struct{ snapshot *zonecache.Snapshot }

func (c kubeRPZFixedConfig) Snapshot() *zonecache.Snapshot { return c.snapshot }
