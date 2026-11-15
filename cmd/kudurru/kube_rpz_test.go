package main

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
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

// Identification must reject both forms of the empty startup snapshot. The
// next stage could see a live zone snapshot even when identification saw the
// preceding unsynced policy snapshot, so authority's own gate is insufficient.
func TestKubeRPZPolicyStartupCapture(t *testing.T) {
	for _, withFeed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no policy", true: "required feed"}[withFeed], func(t *testing.T) {
			st := memory.New()
			t.Cleanup(func() { _ = st.Close() })
			if withFeed {
				for _, obj := range []store.Object{
					&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "all"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
					&public.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"all"}, BlockListRefs: []string{"feed"}}},
					&public.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "feed"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{"127.0.0.1:5300"}}}}}},
				} {
					if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						t.Fatal(err)
					}
				}
			}
			gate := make(chan struct{})
			cache, err := zonecache.New(kubeRPZStartupStore{Store: st, gate: gate}, zonecache.Options{Debounce: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if err := cache.Register(policy.Builder(policy.Options{DefaultNamespace: "default"}, nil)); err != nil {
				t.Fatal(err)
			}
			published := make(chan *zonecache.Snapshot, 10)
			cache.OnPublish(func(snap *zonecache.Snapshot) { published <- snap })
			k, err := newKubeRPZ(cache, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			id := identify.New(policy.Source{Cache: cache}, nil)
			id.Capture = k.capturePolicy
			bs, err := blockingstage.New(blockingstage.Options{RPZ: k.capture})
			if err != nil {
				t.Fatal(err)
			}
			forwarded := 0
			query := func() (*pipeline.Response, error) {
				msg := new(dns.Msg).SetQuestion("blocked.rpzlab.net.", dns.TypeA)
				req := &pipeline.Request{Ctx: t.Context(), Msg: msg, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53000")}}
				return id.Serve(req, func(req *pipeline.Request) (*pipeline.Response, error) {
					return bs.Serve(req, func(req *pipeline.Request) (*pipeline.Response, error) {
						forwarded++
						return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
					})
				})
			}
			assertClosed := func() {
				t.Helper()
				if _, err := query(); !errors.Is(err, pipeline.ErrServingCapture) || forwarded != 0 {
					t.Fatalf("startup query: err=%v forwarded=%d", err, forwarded)
				}
			}
			assertClosed() // No builder has run yet.
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); _ = cache.Start(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			awaitSnapshot := func(state zonecache.State) *zonecache.Snapshot {
				t.Helper()
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				for {
					select {
					case snap := <-published:
						if snap.State == state {
							return snap
						}
					case <-timer.C:
						t.Fatalf("no %s snapshot published", state)
					}
				}
			}
			unsynced := awaitSnapshot(zonecache.Unsynced)
			if ix, _ := zonecache.View[*policy.Index](unsynced, policy.ViewName); ix == nil {
				t.Fatal("no built empty index; this does not exercise the startup gap")
			}
			assertClosed() // An empty policy index was built while watches waited.
			close(gate)
			live := awaitSnapshot(zonecache.Live)
			resp, err := query()
			if err != nil || resp == nil {
				t.Fatalf("live query: resp=%v err=%v", resp, err)
			}
			if withFeed {
				if resp.Msg.Rcode != dns.RcodeServerFailure || forwarded != 0 {
					t.Fatal("the first live query bypassed its required pending feed")
				}
			} else if resp.Msg.Rcode != dns.RcodeSuccess || forwarded != 1 {
				t.Fatal("a synced configuration without a policy could not forward")
			}
			// Stale saved configurations retain their policy index; only the
			// selected feeds fail closed, as tested by the binding test above.
			stale := *live
			stale.State, stale.Stale = zonecache.FromSnapshot, true
			k.config = kubeRPZFixedConfig{&stale}
			if ix, err := k.capturePolicy(nil); ix == nil || err != nil {
				t.Fatalf("stale policy capture: ix=%v err=%v", ix, err)
			}
		})
	}
}

type kubeRPZStartupStore struct {
	store.Store
	gate <-chan struct{}
}

func (s kubeRPZStartupStore) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (store.Watcher, error) {
	select {
	case <-s.gate:
		return s.Store.Watch(ctx, kind, opts)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
