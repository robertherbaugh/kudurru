package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestServingRuntimeWakesWhenAnAwaitedListBodyIsStored proves the serving
// runtime's wiring of the awaited-body path (D373): a BlockList whose status
// names a published digest that this node's list cache does not hold is
// compiled empty without holding back the generation, and storing that body
// with no store change at all (as a follower that pulls the body by digest
// does) prepares the generation again so the list blocks.
//
// The proof is by attribution, not by a deadline. The fixture runs no list
// fetcher and has no identity provider (whose discovery retries about every
// ten seconds prepare the generation again), so nothing else is due to
// prepare. The test also subscribes to every other cause it can observe (a
// store generation, an authority change, a provider dependency change) before
// it stores the body and fails if any of them fired before the list blocked.
// A publication that blocks with none of them fired can only come from
// DiskCache.OnAwaitedBody wired to Publisher.Wake in newServingRuntime.
// Without that wiring the test fails at its bound every time.
func TestServingRuntimeWakesWhenAnAwaitedListBodyIsStored(t *testing.T) {
	f := newServingCommandFixtureOn(t, newServingRuntimeFixtureProviders(t, false))
	const url = "https://lists.example.test/awaited.txt"
	body := "awaited.example\n"
	sum := sha256.Sum256([]byte(body))
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := tx.Apply(ctx, &public.ClientGroup{ObjectMeta: meta("awaiting"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		stored, err := tx.Apply(ctx, &public.BlockList{ObjectMeta: meta("awaited"), Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "url", URL: url}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		list := stored.(*public.BlockList)
		list.Status.Sources = []public.SourceStatus{{Name: "url", Digest: hex.EncodeToString(sum[:])}}
		if err := tx.UpdateStatus(ctx, list); err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &public.Policy{ObjectMeta: meta("awaiting"), Spec: public.PolicySpec{ClientGroupRefs: []string{"awaiting"}, BlockListRefs: []string{"awaited"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// selected reports whether the published policy is the one written above
	// and whether it blocks the listed name.
	selected := func() (found, blocked bool) {
		if !f.s.publisher.Status().ProtectedReady {
			return false, false
		}
		lease, err := f.s.publisher.Capture()
		if err != nil {
			return false, false
		}
		defer lease.Release()
		v, ok := lease.Bundle().Component("policy")
		if !ok {
			return false, false
		}
		ix, ok := v.(*policy.Index)
		if !ok {
			return false, false
		}
		d := ix.SelectPolicy(pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53")}, f.now)
		if d.Policy != "default/awaiting" || d.Selection == nil {
			return false, false
		}
		return true, d.Selection.Evaluate("awaited.example.").Blocked()
	}
	// Before D373 the candidate failed for the missing body and the
	// generation with the policy was never published.
	awaitServingRuntime(t, f.s, func() bool { found, _ := selected(); return found })
	if _, blocked := selected(); blocked {
		t.Fatal("a body this node does not hold was served")
	}
	// Subscribe before storing the body; a cause that fires from here on
	// would make the next preparation unattributable.
	generation := f.s.opts.Store.GenerationChanges()
	authority := f.s.opts.Store.AuthorityChanges()
	dependencies := f.s.providers.Changes()
	before := f.s.publisher.Status().Served.Publication
	if _, _, err := f.s.opts.Lists.Put(url, strings.NewReader(body), listfetcher.PutOptions{MaxSize: 1024}); err != nil {
		t.Fatal(err)
	}
	stored := time.Now()
	for deadline := stored.Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, blocked := selected(); blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stored body was not served within 10s: the generation was not prepared again (publisher=%+v)", f.s.publisher.Status())
		}
	}
	t.Logf("timing: blocked %s after the body was stored", time.Since(stored).Round(time.Millisecond))
	for name, ch := range map[string]<-chan struct{}{"store generation": generation, "authority": authority, "provider dependencies": dependencies} {
		select {
		case <-ch:
			t.Fatalf("a %s change fired before the list blocked, so the preparation that served the body cannot be attributed to the awaited-body wake", name)
		default:
		}
	}
	if after := f.s.publisher.Status().Served.Publication; after <= before {
		t.Fatalf("publication %d did not advance past %d", after, before)
	}
}
