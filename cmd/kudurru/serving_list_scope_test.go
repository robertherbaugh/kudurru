package main

import (
	"net/netip"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServingListScopeRetainsOriginalPublication(t *testing.T) {
	f := newServingCommandFixture(t)
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	bl := &public.BlockList{ObjectMeta: meta("scoped"), Spec: public.BlockListSpec{ClientGroupRefs: []string{"kids"}, Sources: []public.ListSource{{Inline: []string{`/^held[0-9]+\.scope\.example$/`}}}}}
	for _, o := range []store.Object{
		&public.ClientGroup{ObjectMeta: meta("kids"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}},
		&public.ClientGroup{ObjectMeta: meta("adults"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.2"}}}},
		bl,
		&public.Policy{ObjectMeta: meta("shared"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids", "adults"}, BlockListRefs: []string{"scoped"}}},
	} {
		if _, err := f.s.opts.Store.Apply(t.Context(), o, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	await := func(kids bool) {
		t.Helper()
		awaitServingRuntime(t, f.s, func() bool {
			if !f.s.publisher.Status().ProtectedReady {
				return false
			}
			lease, err := f.s.publisher.Capture()
			if err != nil {
				return false
			}
			defer lease.Release()
			v, ok := lease.Bundle().Component("policy")
			if !ok {
				return false
			}
			ix, ok := v.(*policy.Index)
			if !ok {
				return false
			}
			d := ix.SelectPolicy(pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53")}, f.now)
			return d.Policy == "default/shared" && d.Selection != nil && d.Selection.Evaluate("held42.scope.example.").Blocked() == kids
		})
	}
	await(true)
	id := identify.New(nil, nil)
	id.Capture = func(r *pipeline.Request) (*policy.Index, error) {
		return servingdns.Component[*policy.Index](r, "policy")
	}
	id.CaptureInventory = func(r *pipeline.Request) (*inventory.Snapshot, error) {
		owner, err := servingdns.Component[*servingbuild.InventoryOwner](r, "inventory")
		if err != nil {
			return nil, err
		}
		return owner.Snapshot, nil
	}
	capture := func(ip string) *pipeline.Request {
		t.Helper()
		lease, err := f.s.publisher.Capture()
		if err != nil {
			t.Fatal(err)
		}
		wrapped := servingdns.Wrap(lease)
		t.Cleanup(wrapped.Release)
		return &pipeline.Request{Ctx: t.Context(), Serving: wrapped, Received: f.now, Deadline: f.now.Add(5 * time.Second), Client: pipeline.Client{Addr: netip.MustParseAddrPort(ip + ":53"), Proto: pipeline.ProtoUDP, GroupIDs: []string{"default/forged"}}}
	}
	check := func(r *pipeline.Request, want bool) {
		t.Helper()
		if _, err := id.Serve(r, func(r *pipeline.Request) (*pipeline.Response, error) {
			d := policy.FromRequest(r)
			if d == nil || d.Selection == nil || d.Selection.Evaluate("held42.scope.example.").Blocked() != want {
				t.Error("captured list scope changed")
			}
			if r.Deadline != f.now.Add(5*time.Second) || r.Received != f.now {
				t.Error("scope selection renewed original deadline/time")
			}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	type held struct {
		pending, selected *pipeline.Request
		want              bool
	}
	requests := []held{{capture("192.0.2.1"), capture("192.0.2.1"), true}, {capture("192.0.2.2"), capture("192.0.2.2"), false}}
	for _, h := range requests {
		check(h.selected, h.want)
	}
	bl.Spec.ClientGroupRefs = []string{"adults"}
	if _, err := f.s.opts.Store.Apply(t.Context(), bl, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	await(false)
	for _, h := range requests {
		original := h.selected.Policy
		h.selected.Client.GroupIDs = []string{"default/forged"}
		for range 3 {
			check(h.pending, h.want)
			check(h.selected, h.want)
		}
		if h.selected.Policy != original {
			t.Fatal("held decision replaced after publication")
		}
	}
	check(capture("192.0.2.1"), false)
	check(capture("192.0.2.2"), true)
}
