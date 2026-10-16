package dhcpdns

import (
	"context"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func assertOwner(t *testing.T, r *Resolver, name string, address netip.Addr) {
	t.Helper()
	resp := dnsTestAssert(t, r, name, dns.TypeA, dns.RcodeSuccess, 1)
	if got := resp.Msg.Answer[0].(*dns.A).A.String(); got != address.String() {
		t.Fatalf("owner of %s = %s, want %s", name, got, address)
	}
}
func TestOwnershipAcrossSharedScopesRestartAndReservations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	firstCfg := dnsTestConfig()
	firstCfg.Name = "alpha"
	secondCfg := firstCfg
	secondCfg.Name = "zeta"
	secondCfg.Subnet = netip.MustParsePrefix("198.51.100.0/24")
	secondCfg.ServerIP = netip.MustParseAddr("198.51.100.1")
	secondCfg.Router = netip.MustParseAddr("198.51.100.254")
	secondCfg.RangeStart = netip.MustParseAddr("198.51.100.10")
	secondCfg.RangeEnd = netip.MustParseAddr("198.51.100.20")
	paths := []string{filepath.Join(dir, "alpha.db"), filepath.Join(dir, "zeta.db")}
	first, err := dhcp.OpenLeases(paths[0], firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := dhcp.OpenLeases(paths[1], secondCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	// Establish the lexically later scope first. Neither scope order nor IP
	// order may let a subsequently established claimant suppress this binding.
	older := dnsTestClient(9, "shared")
	newer := dnsTestClient(1, "shared")
	old := dnsTestBind(t, second, older)
	fresh := dnsTestBind(t, first, newer)
	r := New(map[string]*dhcp.LeaseStore{"alpha": first, "zeta": second})
	assertOwner(t, r, "shared.leases.test.", old.Address)
	if len(r.Hostnames(fresh.Address)) != 0 {
		t.Fatal("losing claim leaked into policy")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	// Reverse startup order; durable rank must still select the earlier owner.
	first, err = dhcp.OpenLeases(paths[0], firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err = dhcp.OpenLeases(paths[1], secondCfg)
	if err != nil {
		t.Fatal(err)
	}
	r = New(map[string]*dhcp.LeaseStore{"alpha": first, "zeta": second})
	assertOwner(t, r, "shared.leases.test.", old.Address)
	reserved := dnsTestClient(3, "ignored")
	reservation := dhcp.Reservation{MAC: reserved.MAC, IP: netip.MustParseAddr("192.0.2.90"), Hostname: "shared"}
	if err := first.PutReservation(ctx, 1, reservation); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "shared.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	bound := dnsTestBind(t, first, reserved)
	assertOwner(t, r, "shared.leases.test.", bound.Address)
	if err := first.Release(ctx, reserved, bound.Address); err != nil {
		t.Fatal(err)
	}
	dnsTestAssert(t, r, "shared.leases.test.", dns.TypeA, dns.RcodeNameError, 0)
	if err := first.DeleteReservation(ctx, 2, reserved.MAC); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, r, "shared.leases.test.", old.Address)
	if err := second.Release(ctx, older, old.Address); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, r, "shared.leases.test.", fresh.Address)
}

func TestOwnershipNativeImportPreservesOrderWithoutStealingLiveName(t *testing.T) {
	ctx := context.Background()
	cfg := dnsTestConfig()
	source, _ := dnsTestStore(t, cfg)
	older, newer := dnsTestClient(9, "shared"), dnsTestClient(1, "shared")
	offered, err := source.Offer(ctx, older, cfg.RangeEnd)
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.Acknowledge(ctx, older, offered.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	dnsTestBind(t, source, newer)
	document, err := source.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if document.Leases[0].MAC != newer.MAC {
		t.Fatal("fixture did not put newer claimant first in address order")
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := dnsTestStore(t, cfg)
	live := dnsTestClient(7, "shared")
	held, err := target.Offer(ctx, live, cfg.RangeStart.Next())
	if err != nil {
		t.Fatal(err)
	}
	current, err := target.Acknowledge(ctx, live, held.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	r := New(map[string]*dhcp.LeaseStore{"default": target})
	if _, err := target.Import(ctx, "native", data); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, r, "shared.leases.test.", current.Address)
	if err := target.Release(ctx, live, current.Address); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, r, "shared.leases.test.", first.Address)
	// Reimporting the old backup cannot change established rank or identity.
	if _, err := target.Import(ctx, "native", data); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, r, "shared.leases.test.", first.Address)
}

func TestOwnershipExpiryPromotesWaitingClaimWithoutDatabaseCleanup(t *testing.T) {
	cfg := dnsTestConfig()
	s, _ := dnsTestStore(t, cfg)
	now := time.Now().UTC().Truncate(time.Second)
	c1, c2 := dnsTestClient(1, "shared"), dnsTestClient(2, "shared")
	record := dhcp.LeaseRecord{Address: cfg.RangeStart, Identity: dhcp.LeaseIdentity{Kind: "mac", Value: c1.MAC}, MAC: c1.MAC, Hostname: c1.Hostname, State: "bound", ExpiresAt: now.Add(10 * time.Second)}
	document := dhcp.LeaseDocument{Version: 1, Scope: cfg.Name, Subnet: cfg.Subnet, Leases: []dhcp.LeaseRecord{record}}
	data, _ := json.Marshal(document)
	if _, err := s.Import(context.Background(), "native", data); err != nil {
		t.Fatal(err)
	}
	waiting := dnsTestBind(t, s, c2)
	r := New(map[string]*dhcp.LeaseStore{"default": s})
	r.now = func() time.Time { return now }
	assertOwner(t, r, "shared.leases.test.", record.Address)
	r.now = func() time.Time { return now.Add(10 * time.Second) }
	assertOwner(t, r, "shared.leases.test.", waiting.Address)
	if names := r.Hostnames(record.Address); len(names) != 0 {
		t.Fatal("expired owner leaked policy name")
	}
}
