package dhcp

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestLeaseDNSSnapshotCommitBoundaryAndImmutability(t *testing.T) {
	s, _ := leaseTestOpen(t, managedTestConfig())
	c := leaseTestClient(1)
	l := leaseTestBind(t, s, c)
	before := s.DNS()
	if record, ok := before.Address(l.Address); !ok || record.Name != "client-1.leases.test." {
		t.Fatalf("bound snapshot: %+v %v", record, ok)
	}
	leaseTestSQL(t, s, `CREATE TRIGGER fail_release BEFORE DELETE ON leases BEGIN SELECT RAISE(ABORT,'injected'); END`)
	if err := s.Release(context.Background(), c, l.Address); err == nil {
		t.Fatal("failed release succeeded")
	}
	if s.DNS() != before {
		t.Fatal("failed release published DNS")
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_release")
	if err := s.Release(context.Background(), c, l.Address); err != nil {
		t.Fatal(err)
	}
	after := s.DNS()
	if after.Generation() <= before.Generation() {
		t.Fatal("snapshot generation did not advance")
	}
	if _, ok := after.Address(l.Address); ok {
		t.Fatal("released binding retained")
	}
	if _, ok := before.Address(l.Address); !ok {
		t.Fatal("published old snapshot mutated")
	}
	// Snapshot reads remain available without the allocator mutex or SQLite.
	// Close the underlying DB directly, leaving the committed view installed.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if got := s.DNS(); got != after {
		s.mu.Unlock()
		t.Fatal("snapshot read changed with unavailable database")
	}
	_, ok := before.Address(l.Address)
	s.mu.Unlock()
	if !ok {
		t.Fatal("detached snapshot depends on DB")
	}
}

func TestLeaseManagedShorterDurationPreservesLostACKSafety(t *testing.T) {
	cfg := managedTestConfig()
	cfg.RangeEnd = cfg.RangeStart
	s, _ := leaseTestOpen(t, cfg)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	c := leaseTestClient(1)
	bound := leaseTestBind(t, s, c)
	opts := optionsFor(s.Configuration().Config)
	opts.LeaseDuration = time.Minute
	if err := s.UpdateOptions(context.Background(), 1, opts); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []RequestMode{Selecting, InitReboot, Renewing} {
		if ack, err := s.Acknowledge(context.Background(), c, bound.Address, mode); err != nil || ack.ExpiresAt != bound.ExpiresAt {
			t.Fatalf("mode %d shortened durable expiry: %+v %v", mode, ack, err)
		}
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.Offer(context.Background(), leaseTestClient(2), bound.Address); err == nil {
		t.Fatal("lost ACK allowed early address reuse")
	}
}

// D397: every bound lease publishes its MAC identity, including a lease with
// no hostname in a scope that does not register DNS. Old snapshots never change.
func TestLeaseBoundIdentityWithoutDNSRegistration(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	c := Client{MAC: "02:00:00:00:aa:01"}
	offer := leaseTestOffer(t, s, c, netip.Addr{})
	if _, ok := s.DNS().Bound(offer.Address); ok {
		t.Fatal("an offer published a bound identity")
	}
	l := leaseTestBind(t, s, c)
	before := s.DNS()
	if before.Enabled() {
		t.Fatal("fixture scope registers DNS")
	}
	got, ok := before.Bound(l.Address)
	if !ok || got.MAC != c.MAC || got.Hostname != "" || !got.ExpiresAt.Equal(l.ExpiresAt) {
		t.Fatalf("bound identity: %+v %v", got, ok)
	}
	if _, ok := before.Address(l.Address); ok {
		t.Fatal("a nameless lease became a DNS candidate")
	}
	other := leaseTestBind(t, s, leaseTestClient(2))
	count := 0
	s.DNS().ForBound(func(BoundLease) { count++ })
	if count != 2 {
		t.Fatalf("bound leases enumerated: %d", count)
	}
	if _, ok := before.Bound(other.Address); ok {
		t.Fatal("published snapshot mutated by a later binding")
	}
	if err := s.Release(context.Background(), c, l.Address); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.DNS().Bound(l.Address); ok {
		t.Fatal("released lease kept its identity")
	}
	if _, ok := before.Bound(l.Address); !ok {
		t.Fatal("release mutated an older snapshot")
	}
	if _, ok := s.DNS().Bound(netip.MustParseAddr("2001:db8::1")); ok {
		t.Fatal("IPv6 lookup answered")
	}
}
