package identify

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

type leaseNames struct {
	names []string
	calls int
}

func (h *leaseNames) Hostnames(netip.Addr) []string { h.calls++; return h.names }

func TestDHCPNamesAlongsideCapturedInventory(t *testing.T) {
	ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{{ObjectMeta: metav1.ObjectMeta{Name: "lease-client"}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{Hostname: "dhcp.lan"}}}}}}, policy.Options{Inventory: true})
	st := New(policy.Static{Idx: ix}, nil)
	hosts := &leaseNames{names: []string{"dhcp.lan"}}
	st.Hostnames = hosts
	st.CaptureInventory = func(*pipeline.Request) (*inventory.Snapshot, error) { return nil, nil }
	req := &pipeline.Request{Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.9:53")}}
	next := func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }
	if _, err := st.Serve(req, next); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.Client.GroupIDs, []string{"lease-client"}) {
		t.Fatalf("DHCP identity lost: %+v", req.Client)
	}
	hosts.names[0] = "replacement.lan"
	if _, err := st.Serve(req, next); err != nil {
		t.Fatal(err)
	}
	if hosts.calls != 1 || !slices.Equal(req.Client.GroupIDs, []string{"lease-client"}) {
		t.Fatal("dependency changed captured DHCP identity")
	}
	fresh := &pipeline.Request{Client: pipeline.Client{Addr: req.Client.Addr}}
	if _, err := st.Serve(fresh, next); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Client.GroupIDs) != 0 || hosts.calls != 2 {
		t.Fatal("fresh request retained former DHCP name")
	}
}

// PTR observations are a last fallback and are sampled once for the original
// request. Alias/dependency work cannot pick a newer name or policy membership.
func TestFallbackHostnamePrecedenceAndCapture(t *testing.T) {
	group := func(name string, m v1alpha1.ClientMatcher) *v1alpha1.ClientGroup {
		return &v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{m}}}
	}
	ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{
		group("ptr", v1alpha1.ClientMatcher{NameGlob: "*.ptr.lan"}),
		group("dhcp", v1alpha1.ClientMatcher{Hostname: "phone.dhcp.lan"}),
		group("inventory", v1alpha1.ClientMatcher{Hostname: "phone.inventory.lan"}),
		group("exact-ip", v1alpha1.ClientMatcher{IP: "192.0.2.9"}),
	}}, policy.Options{Inventory: true})
	now := time.Now()
	for _, source := range []string{"ptr", "dhcp", "inventory", "exact-ip"} {
		t.Run(source, func(t *testing.T) {
			st := New(policy.Static{Idx: ix}, nil)
			calls := 0
			name := "phone.ptr.lan"
			st.FallbackHostnames = fallbackNames(func(netip.Addr) []string { calls++; return []string{name} })
			addr := netip.MustParseAddr("192.0.2.3")
			if source == "dhcp" {
				st.Hostnames = fallbackNames(func(netip.Addr) []string { return []string{"phone.dhcp.lan"} })
			}
			if source == "inventory" {
				inv := inventory.New(nil)
				if err := inv.Replace("test", 1, []inventory.Client{{ID: "phone", IPs: []netip.Addr{addr}, Hostname: "phone.inventory.lan", ObservedAt: now, ExpiresAt: now.Add(time.Minute)}}); err != nil {
					t.Fatal(err)
				}
				st.Inventory = inv
			}
			if source == "exact-ip" {
				addr = netip.MustParseAddr("192.0.2.9")
			}
			req := &pipeline.Request{Client: pipeline.Client{Addr: netip.AddrPortFrom(addr, 53)}}
			next := func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }
			if _, err := st.Serve(req, next); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(req.Client.GroupIDs, []string{source}) {
				t.Fatal("wrong identity precedence", req.Client.GroupIDs)
			}
			before := calls
			name = "replacement.ptr.lan"
			if _, err := st.Serve(req, next); err != nil {
				t.Fatal(err)
			}
			if calls != before || !slices.Equal(req.Client.GroupIDs, []string{source}) {
				t.Fatal("dependent identity recaptured")
			}
			if (source == "dhcp" || source == "inventory") && calls != 0 {
				t.Fatal("PTR acquired despite authoritative local name")
			}
		})
	}
}

type fallbackNames func(netip.Addr) []string

func (f fallbackNames) Hostnames(addr netip.Addr) []string { return f(addr) }

type leaseMACs struct {
	macs  map[netip.Addr]string
	until time.Time
	calls int
}

func (l *leaseMACs) LeaseMAC(addr netip.Addr, at time.Time) (string, bool) {
	l.calls++
	mac, ok := l.macs[addr]
	return mac, ok && at.Before(l.until)
}

// D397: a live DHCP lease fills the MAC when the captured inventory supplied
// none, so a ClientGroup mac matcher selects the group. Inventory MACs win,
// and a source-bound inventory grant never matches a lease MAC.
func TestLeaseMACFillsOnlyWithoutInventoryMACs(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	leased, inventoried := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
	inv := inventory.New(func() time.Time { return now })
	if err := inv.Replace("unifi", 1, []inventory.Client{{ID: "tv", IPs: []netip.Addr{inventoried}, MACs: []string{"02:00:00:00:00:11"}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	group := func(name, uid string, matchers ...v1alpha1.ClientMatcher) *v1alpha1.ClientGroup {
		return &v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}, Spec: v1alpha1.ClientGroupSpec{Clients: matchers}}
	}
	ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{
		group("default", "u-default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}),
		group("laptop", "u-laptop", v1alpha1.ClientMatcher{MAC: "02-00-00-00-00-10"}),
		group("tv", "u-tv", v1alpha1.ClientMatcher{MAC: "02:00:00:00:00:11"}),
		group("granted", "u-granted"),
	}, MACGrants: []policy.MACGrant{{Group: "granted", GroupUID: "u-granted", MAC: "02:00:00:00:00:12", Source: "unifi", ExpiresAt: now.Add(time.Hour)}}}, policy.Options{Inventory: true})
	st := New(policy.Static{Idx: ix}, func() time.Time { return now })
	st.CaptureInventory = func(*pipeline.Request) (*inventory.Snapshot, error) { return inv.Snapshot(), nil }
	leases := &leaseMACs{macs: map[netip.Addr]string{leased: "02:00:00:00:00:10", inventoried: "02:00:00:00:00:10"}, until: now.Add(time.Minute)}
	st.LeaseMACs = leases
	next := func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }
	serve := func(addr netip.Addr) pipeline.Client {
		t.Helper()
		req := &pipeline.Request{Client: pipeline.Client{Addr: netip.AddrPortFrom(addr, 53)}, Received: now}
		if _, err := st.Serve(req, next); err != nil {
			t.Fatal(err)
		}
		return req.Client
	}
	if c := serve(leased); !slices.Equal(c.GroupIDs, []string{"laptop"}) || !slices.Equal(c.MACs, []string{"02:00:00:00:00:10"}) || c.InventorySource != "" {
		t.Fatalf("lease MAC did not select the mac group: %+v", c)
	}
	if c := serve(inventoried); !slices.Equal(c.GroupIDs, []string{"tv"}) || c.InventorySource != "unifi" {
		t.Fatalf("lease MAC overrode the inventory MAC: %+v", c)
	}
	// The grant's MAC is leased to an address the grant source also lists
	// without a MAC: the lease fills the MAC but must not satisfy the grant.
	granted := netip.MustParseAddr("192.0.2.12")
	if err := inv.Replace("unifi", 2, []inventory.Client{{ID: "tv", IPs: []netip.Addr{inventoried}, MACs: []string{"02:00:00:00:00:11"}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)}, {ID: "anon", IPs: []netip.Addr{granted}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	leases.macs[granted] = "02:00:00:00:00:12"
	if c := serve(granted); !slices.Equal(c.GroupIDs, []string{"default"}) || c.InventorySource != "" {
		t.Fatalf("source-bound grant matched a lease MAC: %+v", c)
	}
	now = now.Add(time.Minute)
	if c := serve(leased); !slices.Equal(c.GroupIDs, []string{"default"}) || len(c.MACs) != 0 {
		t.Fatalf("expired lease still identified: %+v", c)
	}
}
