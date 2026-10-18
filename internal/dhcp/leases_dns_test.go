package dhcp

import (
	"context"
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
