package identify

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

func index() *policy.Index {
	m := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	return policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			{ObjectMeta: m("default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
			{ObjectMeta: m("kids"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.20.0/24"}}}},
		},
		Policies: []*v1alpha1.Policy{
			{ObjectMeta: m("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}},
			{ObjectMeta: m("kids"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, Priority: 10,
				Schedule: &v1alpha1.Schedule{Weekdays: []string{"mon"}, Start: "08:00", End: "12:00"}}},
		},
	}, policy.Options{})
}

func TestInventoryIdentityUsesOneLiveSnapshot(t *testing.T) {
	now := time.Now()
	inv := inventory.New(func() time.Time { return now })
	ip := netip.MustParseAddr("192.0.2.9")
	client := inventory.Client{ID: "phone", IPs: []netip.Addr{ip}, MACs: []string{"02:00:00:00:00:09"}, Hostname: "phone.lan", Tags: []string{"child"}, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := inv.Replace("test", 1, []inventory.Client{client}); err != nil {
		t.Fatal(err)
	}
	group := func(name string, matcher v1alpha1.ClientMatcher) *v1alpha1.ClientGroup {
		return &v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{matcher}}}
	}
	ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{
		group("default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}),
		group("device", v1alpha1.ClientMatcher{MAC: client.MACs[0]}),
		group("hostname", v1alpha1.ClientMatcher{Hostname: client.Hostname}),
		group("remote", v1alpha1.ClientMatcher{ClientID: "phone"}),
	}}, policy.Options{Inventory: true})
	st := New(policy.Static{Idx: ix}, func() time.Time { return now })
	st.Inventory = inv
	next := func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }
	req := &pipeline.Request{Client: pipeline.Client{Addr: netip.AddrPortFrom(ip, 53), ClientID: "phone"}}
	if _, err := st.Serve(req, next); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.Client.GroupIDs, []string{"device"}) || !slices.Equal(req.Client.Tags, []string{"child"}) {
		t.Fatalf("identity not applied: %+v", req.Client)
	}
	now = now.Add(time.Minute)
	if _, err := st.Serve(req, next); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.Client.GroupIDs, []string{"device"}) {
		t.Fatal("captured request identity changed during dependency resolution")
	}
	fresh := &pipeline.Request{Client: pipeline.Client{Addr: req.Client.Addr, ClientID: "phone"}}
	if _, err := st.Serve(fresh, next); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Client.MACs) != 0 || !slices.Equal(fresh.Client.GroupIDs, []string{"remote"}) {
		t.Fatalf("stale inventory masked ClientID: %+v", fresh.Client)
	}
}

func TestServe(t *testing.T) {
	monday9 := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	st := New(policy.Static{Idx: index()}, func() time.Time { return monday9 })
	if st.Name() != "identify" {
		t.Fatal(st.Name())
	}
	var seen *pipeline.Request
	next := func(r *pipeline.Request) (*pipeline.Response, error) {
		seen = r
		return &pipeline.Response{}, nil
	}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)

	cases := []struct {
		from     string
		received time.Time
		groups   []string
		policy   string
	}{
		{"10.0.20.5", time.Time{}, []string{"default/kids"}, "default/kids"},                   // the stage clock: Monday 09:00
		{"10.0.20.5", monday9.Add(4 * time.Hour), []string{"default/kids"}, "default/default"}, // Received wins: 13:00 is outside
		{"192.168.1.1", monday9, []string{"default/default"}, "default/default"},
	}
	for _, tc := range cases {
		req := &pipeline.Request{Msg: q, Received: tc.received, Client: pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(tc.from), 1)}}
		if _, err := st.Serve(req, next); err != nil {
			t.Fatal(err)
		}
		d := policy.FromRequest(seen)
		if !slices.Equal(seen.Client.GroupIDs, tc.groups) || d == nil || d.Policy != tc.policy {
			t.Errorf("%s at %v: groups %v policy %v, want %v %s", tc.from, tc.received, seen.Client.GroupIDs, d, tc.groups, tc.policy)
		}
	}
}

func TestServeWithoutIndex(t *testing.T) {
	for _, st := range []*Stage{New(nil, nil), New(policy.Static{}, nil)} {
		req := &pipeline.Request{Client: pipeline.Client{Addr: netip.MustParseAddrPort("10.0.20.5:1")}}
		called := false
		if _, err := st.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { called = true; return nil, nil }); err != nil {
			t.Fatal(err)
		}
		if !called || req.Policy != nil || req.Client.GroupIDs != nil {
			t.Errorf("no index: pass through untouched (called=%v policy=%v groups=%v)", called, req.Policy, req.Client.GroupIDs)
		}
	}
}

func BenchmarkServe(b *testing.B) {
	st := New(policy.Static{Idx: index()}, nil)
	next := func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	req := &pipeline.Request{Received: at, Client: pipeline.Client{Addr: netip.MustParseAddrPort("10.0.20.5:1")}}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = st.Serve(req, next)
	}
}
