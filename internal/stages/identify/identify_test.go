package identify

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
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
