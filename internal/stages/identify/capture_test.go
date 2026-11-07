package identify

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type identityLease struct{}

func (identityLease) Value() any                              { return struct{}{} }
func (l identityLease) Clone() (pipeline.ServingLease, error) { return l, nil }
func (identityLease) Release()                                {}

type forbiddenIndex struct{}

func (forbiddenIndex) Index() *policy.Index { panic("captured identify consulted live policy") }

type forbiddenInventory struct{}

func (forbiddenInventory) Snapshot() *inventory.Snapshot {
	panic("captured identify consulted live inventory")
}

func TestCaptureKnownEmptyInventoryAndPolicyFailClosed(t *testing.T) {
	s := New(forbiddenIndex{}, nil)
	s.Inventory = forbiddenInventory{}
	ix := index()
	s.Capture = func(*pipeline.Request) (*policy.Index, error) { return ix, nil }
	s.CaptureInventory = func(*pipeline.Request) (*inventory.Snapshot, error) { return nil, nil }
	r := &pipeline.Request{Serving: identityLease{}, Received: time.Now(), Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53"), Names: []string{"stale"}, Tags: []string{"child"}}}
	called := 0
	next := func(*pipeline.Request) (*pipeline.Response, error) { called++; return nil, nil }
	if _, err := s.Serve(r, next); err != nil {
		t.Fatal(err)
	}
	if !r.Client.IdentityCaptured || len(r.Client.Names) != 0 || len(r.Client.Tags) != 0 || r.Policy == nil {
		t.Fatal("known-empty capture lost", r.Client)
	}
	// Subquery must not re-enrich a known-empty identity after a live update.
	s.CaptureInventory = func(*pipeline.Request) (*inventory.Snapshot, error) {
		t.Fatal("recaptured inherited identity")
		return nil, nil
	}
	if _, err := s.Serve(r, next); err != nil {
		t.Fatal(err)
	}
	s.Capture = nil
	if _, err := s.Serve(r, next); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("live policy fallback", err)
	}
	if called != 2 {
		t.Fatal("failed capture reached next")
	}
}

func TestEarlyExemptionPinsOriginalMembershipPolicyAndIdentity(t *testing.T) {
	at := time.Now()
	group := &v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "device", Namespace: "default", UID: "original"}, Spec: v1alpha1.ClientGroupSpec{RateLimitExempt: true}}
	grant := policy.MACGrant{Group: "default/device", GroupUID: "original", MAC: "02:00:00:00:00:01", Source: "trusted", ExpiresAt: at.Add(time.Second)}
	old := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{group}, MACGrants: []policy.MACGrant{grant}}, policy.Options{Inventory: true})
	group.Spec.RateLimitExempt = false
	fresh := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{group}, MACGrants: []policy.MACGrant{grant}}, policy.Options{Inventory: true})
	st := New(forbiddenIndex{}, func() time.Time { return at.Add(time.Hour) })
	st.Inventory = forbiddenInventory{}
	captures := 0
	st.Capture = func(*pipeline.Request) (*policy.Index, error) { captures++; return old, nil }
	// This trusted identity was already taken from the original immutable owner.
	st.CaptureInventory = func(*pipeline.Request) (*inventory.Snapshot, error) {
		t.Fatal("recaptured original identity")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := &pipeline.Request{Ctx: ctx, Serving: identityLease{}, Received: at, Deadline: at.Add(5 * time.Second), Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53"), IdentityCaptured: true, InventorySource: "trusted", MACs: []string{grant.MAC}}}
	if exempt, err := st.RateLimitExempt(req); err != nil || !exempt {
		t.Fatal("early captured membership unavailable", err)
	}
	if req.Policy != nil || req.Ctx.Value(st).(*identification).decision != nil {
		t.Fatal("early exemption performed policy/schedule work")
	}
	if _, err := st.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	decision := policy.FromRequest(req)
	// A concurrent successor, source change or later clock cannot replace the
	// result pinned at ingress or renew the private grant's exclusive deadline.
	st.Capture = func(*pipeline.Request) (*policy.Index, error) { captures++; return fresh, nil }
	cancel()
	called := false
	if _, err := st.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
		called = true
		if r.Policy != decision || r.Received != at || r.Deadline != at.Add(5*time.Second) || !errors.Is(r.Ctx.Err(), context.Canceled) {
			t.Error("early identification changed original proof/time/context")
		}
		return nil, nil
	}); err != nil || !called || captures != 1 {
		t.Fatal("normal identify recaptured successor", err, captures)
	}
	next := &pipeline.Request{Serving: identityLease{}, Received: at, Client: req.Client}
	if exempt, err := st.RateLimitExempt(next); err != nil || exempt {
		t.Fatal("fresh request adopted old exemption", err)
	}
	st.Capture = func(*pipeline.Request) (*policy.Index, error) { return old, nil }
	expired := &pipeline.Request{Serving: identityLease{}, Received: grant.ExpiresAt, Client: req.Client}
	if exempt, err := st.RateLimitExempt(expired); err != nil || exempt {
		t.Fatal("fresh request renewed expired source grant", err)
	}
}
