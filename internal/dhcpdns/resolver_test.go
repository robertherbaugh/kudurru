package dhcpdns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func dnsTestConfig() dhcp.Config {
	return dhcp.Config{Name: "default", Management: "api", Interface: "eth0", ServerIP: netip.MustParseAddr("192.0.2.1"), Subnet: netip.MustParsePrefix("192.0.2.0/24"), RangeStart: netip.MustParseAddr("192.0.2.10"), RangeEnd: netip.MustParseAddr("192.0.2.20"), Router: netip.MustParseAddr("192.0.2.254"), DNSServers: []netip.Addr{netip.MustParseAddr("192.0.2.1")}, LeaseDuration: time.Hour, DomainName: "leases.test", RegisterDNS: true, DNSTTL: 30 * time.Second}
}
func dnsTestStore(t *testing.T, cfg dhcp.Config) (*dhcp.LeaseStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dhcp.db")
	s, err := dhcp.OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}
func dnsTestClient(n int, hostname string) dhcp.Client {
	return dhcp.Client{MAC: fmt.Sprintf("02:00:00:00:00:%02x", n), Hostname: hostname}
}
func dnsTestBind(t *testing.T, s *dhcp.LeaseStore, c dhcp.Client) dhcp.Lease {
	t.Helper()
	offer, err := s.Offer(context.Background(), c, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Acknowledge(context.Background(), c, offer.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func dnsTestQuery(r *Resolver, name string, qtype uint16) (*pipeline.Response, bool) {
	msg := new(dns.Msg)
	msg.SetQuestion(name, qtype)
	return r.ResolveLocal(&pipeline.Request{Msg: msg})
}
func dnsTestAssert(t *testing.T, r *Resolver, name string, qtype uint16, rcode, answers int) *pipeline.Response {
	t.Helper()
	resp, handled := dnsTestQuery(r, name, qtype)
	if !handled || resp.Msg.Rcode != rcode || len(resp.Msg.Answer) != answers {
		t.Fatalf("query %s/%d: handled=%v response=%+v", name, qtype, handled, resp)
	}
	if qtype != dns.TypeAXFR && qtype != dns.TypeIXFR && (!resp.Msg.Authoritative || resp.Upstream != "dhcp" || resp.Cacheable) {
		t.Fatalf("unsafe local metadata: %+v", resp)
	}
	return resp
}

func TestResolverLeaseLifecycleAndRestart(t *testing.T) {
	cfg := dnsTestConfig()
	s, path := dnsTestStore(t, cfg)
	c := dnsTestClient(1, "Laptop")
	r := New(map[string]*dhcp.LeaseStore{"default": s})
	offer, err := s.Offer(context.Background(), c, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "laptop.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	lease, err := s.Acknowledge(context.Background(), c, offer.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	response := dnsTestAssert(t, r, "LAPTOP.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	if a := response.Msg.Answer[0].(*dns.A); a.A.String() != lease.Address.String() || a.Hdr.Ttl != 30 {
		t.Fatalf("A: %v", a)
	}
	reverse, _ := dns.ReverseAddr(lease.Address.String())
	response = dnsTestAssert(t, r, reverse, dns.TypePTR, dns.RcodeSuccess, 1)
	if response.Msg.Answer[0].(*dns.PTR).Ptr != "laptop.leases.test." {
		t.Fatal(response.Msg)
	}
	if got := r.Hostnames(lease.Address); len(got) != 1 || got[0] != "laptop.leases.test" {
		t.Fatalf("policy names: %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = dhcp.OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	r = New(map[string]*dhcp.LeaseStore{"default": s})
	dnsTestAssert(t, r, "laptop.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	if err := s.Release(context.Background(), c, lease.Address); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "laptop.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	dnsTestAssert(t, r, reverse, dns.TypePTR, dns.RcodeNameError, 0)
	if len(r.Hostnames(lease.Address)) != 0 {
		t.Fatal("released policy hostname retained")
	}
	lease = dnsTestBind(t, s, c)
	if err := s.Decline(context.Background(), c, lease.Address); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "laptop.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
}

func TestResolverTTLExpiryAndNegativeSemantics(t *testing.T) {
	s, _ := dnsTestStore(t, dnsTestConfig())
	now := time.Now().UTC().Truncate(time.Second)
	cfg := s.Configuration().Config
	c := dnsTestClient(1, "host.branch.leases.test")
	doc := dhcp.LeaseDocument{Version: 1, Scope: cfg.Name, Subnet: cfg.Subnet, Leases: []dhcp.LeaseRecord{{Address: cfg.RangeStart, Identity: dhcp.LeaseIdentity{Kind: "mac", Value: c.MAC}, MAC: c.MAC, Hostname: c.Hostname, State: "bound", ExpiresAt: now.Add(10 * time.Second)}}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(context.Background(), "native", data); err != nil {
		t.Fatal(err)
	}
	r := New(map[string]*dhcp.LeaseStore{"default": s})
	r.now = func() time.Time { return now }
	resp := dnsTestAssert(t, r, "host.branch.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	if resp.Msg.Answer[0].Header().Ttl != 10 {
		t.Fatalf("TTL exceeded expiry: %v", resp.Msg)
	}
	for _, name := range []string{"host.branch.leases.test.", "branch.leases.test.", "leases.test."} {
		resp = dnsTestAssert(t, r, name, dns.TypeAAAA, dns.RcodeSuccess, 0)
		if len(resp.Msg.Ns) != 1 || resp.Msg.Ns[0].Header().Ttl != 0 || resp.Msg.Ns[0].(*dns.SOA).Minttl != 0 {
			t.Fatalf("negative cache TTL: %v", resp.Msg)
		}
	}
	dnsTestAssert(t, r, "missing.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	dnsTestAssert(t, r, "leases.test.", dns.TypeSOA, dns.RcodeSuccess, 1)
	for _, typ := range []uint16{dns.TypeAXFR, dns.TypeIXFR} {
		dnsTestAssert(t, r, "leases.test.", typ, dns.RcodeRefused, 0)
	}
	for _, name := range []string{"outside.test.", "10.100.51.198.in-addr.arpa.", "2.0.192.in-addr.arpa."} {
		if _, ok := dnsTestQuery(r, name, dns.TypePTR); ok {
			t.Fatalf("claimed unrelated namespace %s", name)
		}
	}
	r.now = func() time.Time { return now.Add(9500 * time.Millisecond) }
	resp = dnsTestAssert(t, r, "host.branch.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	if resp.Msg.Answer[0].Header().Ttl != 0 {
		t.Fatalf("fractional remaining TTL rounded up: %v", resp.Msg)
	}
	// No DB cleanup or allocator operation: reads filter expiration themselves.
	r.now = func() time.Time { return now.Add(10 * time.Second) }
	dnsTestAssert(t, r, "host.branch.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	dnsTestAssert(t, r, "branch.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	if len(r.Hostnames(cfg.RangeStart)) != 0 {
		t.Fatal("expired policy name retained")
	}
}

func TestResolverCollisionAndOfflineReservationProtection(t *testing.T) {
	s, _ := dnsTestStore(t, dnsTestConfig())
	r := New(map[string]*dhcp.LeaseStore{"default": s})
	c1, c2 := dnsTestClient(1, "same"), dnsTestClient(2, "same")
	first := dnsTestBind(t, s, c1)
	second := dnsTestBind(t, s, c2)
	dnsTestAssert(t, r, "same.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	reverse, _ := dns.ReverseAddr(first.Address.String())
	dnsTestAssert(t, r, reverse, dns.TypePTR, dns.RcodeSuccess, 1)
	if len(r.Hostnames(second.Address)) != 0 {
		t.Fatal("collision leaked into policy")
	}
	if err := s.Release(context.Background(), c2, second.Address); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "same.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	rsv := dhcp.Reservation{MAC: c2.MAC, IP: netip.MustParseAddr("192.0.2.90"), Hostname: "same"}
	if err := s.PutReservation(context.Background(), 1, rsv); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "same.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	// Reserved name owner wins even with a conflicting live dynamic hostname.
	second = dnsTestBind(t, s, c2)
	resp := dnsTestAssert(t, r, "same.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	if resp.Msg.Answer[0].(*dns.A).A.String() != second.Address.String() {
		t.Fatal("dynamic client stole reserved name")
	}
	if err := s.Release(context.Background(), c2, second.Address); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "same.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	if err := s.DeleteReservation(context.Background(), 2, c2.MAC); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "same.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
}

func TestResolverConfigurationUpdatesAndOutsideNames(t *testing.T) {
	s, _ := dnsTestStore(t, dnsTestConfig())
	r := New(map[string]*dhcp.LeaseStore{"default": s})
	c := dnsTestClient(1, "original")
	l := dnsTestBind(t, s, c)
	if err := s.PutReservation(context.Background(), 1, dhcp.Reservation{MAC: c.MAC, IP: l.Address, Hostname: "reserved"}); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "original.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	dnsTestAssert(t, r, "reserved.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	state := s.Configuration()
	cfg := state.Config
	opts := dhcp.PoolOptions{RangeStart: cfg.RangeStart, RangeEnd: cfg.RangeEnd, Router: cfg.Router, DNSServers: cfg.DNSServers, DomainName: "changed.test", LeaseDuration: cfg.LeaseDuration, RegisterDNS: true, DNSTTL: time.Second}
	if err := s.UpdateOptions(context.Background(), state.Revision, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := dnsTestQuery(r, "reserved.leases.test.", dns.TypeA); ok {
		t.Fatal("old domain still owned")
	}
	resp := dnsTestAssert(t, r, "reserved.changed.test.", dns.TypeA, dns.RcodeSuccess, 1)
	if resp.Msg.Answer[0].Header().Ttl != 1 {
		t.Fatal("updated TTL not visible")
	}
	if err := s.DeleteReservation(context.Background(), s.Configuration().Revision, c.MAC); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "original.changed.test.", dns.TypeA, dns.RcodeSuccess, 1)
	other := dnsTestClient(2, "outside.example")
	outside := dnsTestBind(t, s, other)
	if _, ok := dnsTestQuery(r, "outside.example.", dns.TypeA); ok {
		t.Fatal("outside hostname registered")
	}
	if len(r.Hostnames(outside.Address)) != 0 {
		t.Fatal("outside hostname exposed to policy")
	}
	opts.RegisterDNS = false
	if err := s.UpdateOptions(context.Background(), s.Configuration().Revision, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := dnsTestQuery(r, "original.changed.test.", dns.TypeA); ok {
		t.Fatal("disabled scope answered")
	}
}

func TestResolverMultipleScopesAndLongestDomain(t *testing.T) {
	first, _ := dnsTestStore(t, dnsTestConfig())
	cfg := dnsTestConfig()
	cfg.Name = "second"
	cfg.Subnet = netip.MustParsePrefix("198.51.100.0/24")
	cfg.ServerIP = netip.MustParseAddr("198.51.100.1")
	cfg.Router = netip.MustParseAddr("198.51.100.254")
	cfg.RangeStart = netip.MustParseAddr("198.51.100.10")
	cfg.RangeEnd = netip.MustParseAddr("198.51.100.20")
	second, _ := dnsTestStore(t, cfg)
	r := New(map[string]*dhcp.LeaseStore{"default": first, "second": second})
	dnsTestBind(t, first, dnsTestClient(1, "duplicate"))
	dnsTestBind(t, second, dnsTestClient(2, "duplicate"))
	dnsTestAssert(t, r, "duplicate.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
	dnsTestBind(t, first, dnsTestClient(3, "shadow.child.leases.test"))
	cfg.Name = "child"
	cfg.DomainName = "child.leases.test"
	child, _ := dnsTestStore(t, cfg)
	r = New(map[string]*dhcp.LeaseStore{"default": first, "child": child})
	dnsTestAssert(t, r, "shadow.child.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	dnsTestBind(t, child, dnsTestClient(4, "shadow"))
	dnsTestAssert(t, r, "shadow.child.leases.test.", dns.TypeA, dns.RcodeSuccess, 1)
}

func TestResolverConcurrentSnapshotReaders(t *testing.T) {
	s, _ := dnsTestStore(t, dnsTestConfig())
	r := New(map[string]*dhcp.LeaseStore{"default": s})
	c := dnsTestClient(1, "changing")
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				response, handled := dnsTestQuery(r, "changing.leases.test.", dns.TypeA)
				if !handled || response.Msg.Rcode != dns.RcodeSuccess && response.Msg.Rcode != dns.RcodeNameError {
					t.Error("inconsistent snapshot response")
					return
				}
				_ = r.Hostnames(netip.MustParseAddr("192.0.2.10"))
			}
		})
	}
	for range 20 {
		l := dnsTestBind(t, s, c)
		if err := s.Release(context.Background(), c, l.Address); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
}
