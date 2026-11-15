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

func TestServingClientExemptionRetainsOriginalPublication(t *testing.T) {
	f := newServingCommandFixture(t)
	g := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"}, Spec: public.ClientGroupSpec{RateLimitExempt: true, Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}
	apply := func() {
		t.Helper()
		if _, err := f.s.opts.Store.Apply(t.Context(), g, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool {
			if !f.s.publisher.Status().ProtectedReady {
				return false
			}
			lease, err := f.s.publisher.Capture()
			if err != nil {
				return false
			}
			defer lease.Release()
			value, ok := lease.Bundle().Component("policy")
			if !ok {
				return false
			}
			ix, ok := value.(*policy.Index)
			return ok && ix.IdentifyAt(pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53")}, f.now).RateLimitExempt() == g.Spec.RateLimitExempt
		})
	}
	apply()
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
	capture := func() *pipeline.Request {
		t.Helper()
		lease, err := f.s.publisher.Capture()
		if err != nil {
			t.Fatal(err)
		}
		wrapped := servingdns.Wrap(lease)
		t.Cleanup(wrapped.Release)
		return &pipeline.Request{Ctx: t.Context(), Serving: wrapped, Received: f.now, Deadline: f.now.Add(5 * time.Second), Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53"), Proto: pipeline.ProtoUDP}}
	}
	// Hold before identification as well as after ingress: neither pending work
	// nor an already selected membership may adopt a successor's exemption.
	pending, selected := capture(), capture()
	if exempt, err := id.RateLimitExempt(selected); err != nil || !exempt {
		t.Fatal("original group not exempt", err)
	}
	if _, err := id.Serve(selected, func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	originalPolicy := selected.Policy
	g.Spec.RateLimitExempt = false
	apply()
	for _, held := range []*pipeline.Request{pending, selected} {
		if exempt, err := id.RateLimitExempt(held); err != nil || !exempt {
			t.Fatal("held request adopted successor group policy", err)
		}
	}
	if _, err := id.Serve(selected, func(r *pipeline.Request) (*pipeline.Response, error) {
		if r.Policy != originalPolicy || r.Deadline != f.now.Add(5*time.Second) {
			t.Error("normal identify replaced original policy/deadline")
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if exempt, err := id.RateLimitExempt(capture()); err != nil || exempt {
		t.Fatal("successor adopted old group exemption", err)
	}
}
