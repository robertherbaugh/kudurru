package dhcp

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

type externalRecorder struct {
	mu      sync.Mutex
	batches [][]ExternalChange
	fail    error
}

func (r *externalRecorder) persist(_ context.Context, changes []ExternalChange) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.batches = append(r.batches, changes)
	return nil
}

func (r *externalRecorder) take() [][]ExternalChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.batches
	r.batches = nil
	return out
}

func externalTestOpen(t *testing.T, cfg Config) (*LeaseStore, *externalRecorder) {
	t.Helper()
	r := &externalRecorder{}
	s, err := OpenExternalLeases(filepath.Join(t.TempDir(), "scope.db"), cfg, r.persist)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, r
}

func externalTestLease(ip, mac string, state string, expires time.Time) ExternalLease {
	return ExternalLease{Record: LeaseRecord{Address: netip.MustParseAddr(ip), Identity: LeaseIdentity{Kind: "mac", Value: mac},
		MAC: mac, ExpiresAt: expires.UTC().Truncate(time.Second), State: state}}
}

func TestExternalLeasesPersistBeforeCommit(t *testing.T) {
	s, r := externalTestOpen(t, managedTestConfig())
	ctx := context.Background()
	client := Client{MAC: "02:00:00:00:00:01", Hostname: "laptop"}
	offer, err := s.Offer(ctx, client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	batches := r.take()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Address != offer.Address || batches[0][0].Lease == nil || batches[0][0].Lease.Record.State != "offered" {
		t.Fatalf("offer persisted %+v", batches)
	}
	if _, err := s.Acknowledge(ctx, client, offer.Address, Selecting); err != nil {
		t.Fatal(err)
	}
	batches = r.take()
	if len(batches) != 1 || batches[0][0].Lease.Record.State != "bound" || batches[0][0].Lease.Claim.Ticket == 0 || batches[0][0].Lease.Claim.Name != "laptop.leases.test." {
		t.Fatalf("binding persisted %+v", batches)
	}

	// A rejected external write rolls the local transaction back.
	r.fail = errors.New("apiserver unavailable")
	if err := s.Release(ctx, client, offer.Address); !errors.Is(err, r.fail) {
		t.Fatalf("release error = %v", err)
	}
	leases, err := s.List(ctx)
	if err != nil || len(leases) != 1 || leases[0].State != "bound" {
		t.Fatalf("leases after failed release = %+v, %v", leases, err)
	}
	r.fail = nil
	if err := s.Release(ctx, client, offer.Address); err != nil {
		t.Fatal(err)
	}
	batches = r.take()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Lease != nil || batches[0][0].Address != offer.Address {
		t.Fatalf("release persisted %+v", batches)
	}
}

func TestExternalLeasesDoNotPersistExpiry(t *testing.T) {
	s, r := externalTestOpen(t, managedTestConfig())
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	if _, err := s.Offer(ctx, Client{MAC: "02:00:00:00:00:01"}, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	r.take()
	now = now.Add(2 * offerHold)
	if _, err := s.Offer(ctx, Client{MAC: "02:00:00:00:00:02"}, netip.MustParseAddr("192.0.2.10")); err != nil {
		t.Fatal(err)
	}
	batches := r.take()
	// The expired hold at .10 is reused: the new offer replaces it, and
	// no separate delete is sent for the expired row.
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Lease == nil || batches[0][0].Lease.Record.MAC != "02:00:00:00:00:02" {
		t.Fatalf("persisted %+v", batches)
	}
}

func TestExternalLeasesSyncAndReset(t *testing.T) {
	s, r := externalTestOpen(t, managedTestConfig())
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	bound := externalTestLease("192.0.2.10", "02:00:00:00:00:01", "bound", expires)
	bound.Record.Hostname = "printer"
	bound.Claim = DNSClaim{Name: "printer.leases.test.", Epoch: 7, Ticket: 40}
	if err := s.SyncExternal(ctx, []ExternalLease{bound}, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.DNS().Address(netip.MustParseAddr("192.0.2.10")); !ok || got.Name != "printer.leases.test." {
		t.Fatalf("DNS after sync = %+v, %v", got, ok)
	}
	// The external store moved the client: its old local row goes away.
	moved := externalTestLease("192.0.2.11", "02:00:00:00:00:01", "bound", expires)
	outside := externalTestLease("198.51.100.5", "02:00:00:00:00:09", "bound", expires)
	if err := s.SyncExternal(ctx, []ExternalLease{moved, outside}, nil); err != nil {
		t.Fatal(err)
	}
	leases, err := s.List(ctx)
	if err != nil || len(leases) != 1 || leases[0].Address != netip.MustParseAddr("192.0.2.11") {
		t.Fatalf("leases after move = %+v, %v", leases, err)
	}
	if err := s.SyncExternal(ctx, nil, []netip.Addr{netip.MustParseAddr("192.0.2.11")}); err != nil {
		t.Fatal(err)
	}
	if leases, _ := s.List(ctx); len(leases) != 0 {
		t.Fatalf("leases after delete = %+v", leases)
	}
	if len(r.take()) != 0 {
		t.Fatal("sync must not call the persister")
	}

	// Reset keeps the latest row of a duplicated client and continues claim
	// ranks above every rank it saw.
	older := externalTestLease("192.0.2.10", "02:00:00:00:00:02", "bound", expires)
	newer := externalTestLease("192.0.2.12", "02:00:00:00:00:02", "bound", expires.Add(time.Minute))
	newer.Record.Hostname = "tablet"
	newer.Claim = DNSClaim{Name: "tablet.leases.test.", Epoch: 9, Ticket: 500}
	if err := s.ResetExternal(ctx, []ExternalLease{older, newer}); err != nil {
		t.Fatal(err)
	}
	leases, err = s.List(ctx)
	if err != nil || len(leases) != 1 || leases[0].Address != netip.MustParseAddr("192.0.2.12") {
		t.Fatalf("leases after reset = %+v, %v", leases, err)
	}
	client := Client{MAC: "02:00:00:00:00:03", Hostname: "phone"}
	offer, err := s.Offer(ctx, client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acknowledge(ctx, client, offer.Address, Selecting); err != nil {
		t.Fatal(err)
	}
	batches := r.take()
	claim := batches[len(batches)-1][0].Lease.Claim
	if claim.Epoch != 9 || claim.Ticket <= 500 {
		t.Fatalf("new claim %+v does not follow the external floor", claim)
	}
}

func TestExternalConfig(t *testing.T) {
	s, r := externalTestOpen(t, managedTestConfig())
	ctx := context.Background()
	client := Client{MAC: "02:00:00:00:00:01", Hostname: "laptop"}
	offer, err := s.Offer(ctx, client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acknowledge(ctx, client, offer.Address, Selecting); err != nil {
		t.Fatal(err)
	}
	r.take()

	cfg := managedTestConfig()
	cfg.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: offer.Address, Hostname: "workstation"}}
	if err := s.SetExternalConfig(ctx, cfg, 5, true); err != nil {
		t.Fatal(err)
	}
	batches := r.take()
	if len(batches) != 1 || batches[0][0].Lease.Claim.Name != "workstation.leases.test." {
		t.Fatalf("claim recomputation persisted %+v", batches)
	}
	if got := s.Configuration(); got.Revision != 5 || !reflect.DeepEqual(got.Config.Reservations, cfg.Reservations) {
		t.Fatalf("configuration = %+v", got)
	}

	// A reservation of the bound address for another client conflicts.
	conflict := managedTestConfig()
	conflict.Reservations = []Reservation{{MAC: "02:00:00:00:00:02", IP: offer.Address}}
	if err := s.CheckExternalConfig(conflict); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("conflicting check = %v", err)
	}
	// It still applies when the external store says so; the lease is not
	// renewed and ends at expiry.
	if err := s.SetExternalConfig(ctx, conflict, 6, false); err != nil {
		t.Fatal(err)
	}
	if len(r.take()) != 0 {
		t.Fatal("a replica must not persist claim changes")
	}
	if _, err := s.Acknowledge(ctx, client, offer.Address, Renewing); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("renewal after conflicting settings = %v", err)
	}

	moved := managedTestConfig()
	moved.Subnet = netip.MustParsePrefix("192.0.2.0/25")
	if err := s.SetExternalConfig(ctx, moved, 7, true); err == nil {
		t.Fatal("a topology change must need a new store")
	}
}

func TestServerIdentifierRequiresRelayScope(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.ServerIdentifier = netip.MustParseAddr("203.0.113.67")
	if err := cfg.Validate(); err == nil {
		t.Fatal("a direct scope accepted an advertised server identifier")
	}
	cfg.RelayOnly = true
	cfg.TrustedRelays = []TrustedRelay{{SourceIP: netip.MustParseAddr("192.0.2.254"), GatewayIP: netip.MustParseAddr("192.0.2.254")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: normalizedConfig(cfg)}
	if got := server.serverIdentifier(new(dhcpv4.DHCPv4)); got != cfg.ServerIdentifier {
		t.Fatalf("server identifier = %s", got)
	}
	if !cfg.infrastructure(cfg.ServerIdentifier) {
		t.Fatal("the advertised server identifier must never be allocated")
	}
}
