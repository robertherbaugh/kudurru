package dhcp

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuarantineOfferExactOwnerAndCommit(t *testing.T) {
	ctx := context.Background()
	s, path := leaseTestOpen(t, leaseTestConfig())
	c := leaseTestClient(1)
	c.ID = "opaque"
	offer := leaseTestOffer(t, s, c, netip.Addr{})
	for _, wrong := range []Client{{ID: c.ID, MAC: leaseTestClient(2).MAC}, {MAC: c.MAC}, {ID: "other", MAC: c.MAC}} {
		if err := s.QuarantineOffer(ctx, wrong, offer.Address); !errors.Is(err, ErrUnknownLease) {
			t.Fatalf("wrong owner accepted: %v", err)
		}
	}
	before := s.DNS()
	leaseTestSQL(t, s, `CREATE TRIGGER fail_probe BEFORE UPDATE ON leases BEGIN SELECT RAISE(ABORT,'injected'); END`)
	if err := s.QuarantineOffer(ctx, c, offer.Address); err == nil {
		t.Fatal("failed quarantine acknowledged")
	}
	if s.DNS() != before {
		t.Fatal("failed quarantine published")
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_probe")
	if err := s.QuarantineOffer(ctx, c, offer.Address); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLeases(path, leaseTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	rows := leaseTestList(t, reopened)
	if len(rows) != 1 || rows[0].State != "declined" || rows[0].MAC != c.MAC || rows[0].ClientID != c.ID {
		t.Fatalf("quarantine did not survive: %+v", rows)
	}
	if err := reopened.QuarantineOffer(ctx, c, offer.Address); !errors.Is(err, ErrUnknownLease) {
		t.Fatal(err)
	}
	other := leaseTestClient(2)
	bound := leaseTestBind(t, reopened, other)
	if err := reopened.QuarantineOffer(ctx, other, bound.Address); !errors.Is(err, ErrUnknownLease) {
		t.Fatalf("stale probe quarantined ACK: %v", err)
	}
}

func TestDNSClaimSchemaMigrationRollback(t *testing.T) {
	cfg := managedTestConfig()
	s, path := leaseTestOpen(t, cfg)
	bound := leaseTestBind(t, s, leaseTestClient(1))
	leaseTestSQL(t, s, "DROP TABLE lease_dns_claims; DROP TABLE lease_replica; DROP INDEX leases_expiry; PRAGMA user_version=3")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// File ownership lets an incompatible changed default exercise the migration
	// transaction before any new claims can become committed.
	bad := cfg
	bad.Management = "file"
	bad.RangeStart = bound.Address.Next()
	if opened, err := OpenLeases(path, bad); err == nil {
		_ = opened.Close()
		t.Fatal("unsafe migration succeeded")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version, n int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name IN ('lease_dns_claims','lease_replica','leases_expiry')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if version != 3 || n != 0 {
		t.Fatalf("failed migration left schema writes: version=%d objects=%d", version, n)
	}
	_ = db.Close()
	reopened, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	record, ok := reopened.DNS().Address(bound.Address)
	if !ok || record.ClaimTicket == 0 {
		t.Fatalf("legacy owner not migrated: %+v", record)
	}
	ticket := record.ClaimTicket
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	record, _ = reopened.DNS().Address(bound.Address)
	if record.ClaimTicket != ticket {
		t.Fatal("migration claim was reissued")
	}
}

func TestLeaseCacheReflectsExternalSQLAndTriggerWrites(t *testing.T) {
	cfg := managedTestConfig()
	s, _ := leaseTestOpen(t, cfg)
	c := leaseTestClient(1)
	bound := leaseTestBind(t, s, c)
	old := s.DNS()
	leaseTestSQL(t, s, "UPDATE leases SET hostname='external'")
	// An external SQL edit invalidates total_changes; the next committed read
	// must reflect it rather than trusting the previous allocation index.
	rows := leaseTestList(t, s)
	if rows[0].Hostname != "external" {
		t.Fatalf("stale committed lease cache: %+v", rows)
	}
	// SQL does not issue valid DNS claims. Existing claims are retained until a
	// supported mutation reconciles them, and immutable old snapshots survive.
	if record, ok := old.Address(bound.Address); !ok || record.Name != "client-1.leases.test." {
		t.Fatal("old snapshot mutated")
	}
	leaseTestSQL(t, s, `CREATE TRIGGER extra_change AFTER UPDATE ON leases WHEN NEW.hostname != 'triggered' BEGIN UPDATE leases SET hostname='triggered' WHERE address=NEW.address; END`)
	c.Hostname = "requested"
	if _, err := s.Acknowledge(context.Background(), c, bound.Address, Renewing); err != nil {
		t.Fatal(err)
	}
	rows = leaseTestList(t, s)
	if rows[0].Hostname != "triggered" {
		t.Fatalf("trigger write omitted: %+v", rows)
	}
}

func TestDNSClaimRegistryRecreatedAbovePersistedFloor(t *testing.T) {
	cfg := managedTestConfig()
	s, path := leaseTestOpen(t, cfg)
	first := leaseTestBind(t, s, leaseTestClient(1))
	claim, _ := s.DNS().Address(first.Address)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A newly chosen registry path simulates restoring only the lease file.
	oldRegistry := filepath.Join(filepath.Dir(path), ".dhcp-dns-claims.sqlite")
	backup := oldRegistry + ".old"
	if err := os.Rename(oldRegistry, backup); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	second := leaseTestBind(t, reopened, leaseTestClient(2))
	next, _ := reopened.DNS().Address(second.Address)
	if next.ClaimEpoch < claim.ClaimEpoch || next.ClaimEpoch == claim.ClaimEpoch && next.ClaimTicket <= claim.ClaimTicket {
		t.Fatalf("claim rank reused: %+v then %+v", claim, next)
	}
}

func TestDNSClaimRenewalAndCanonicalRename(t *testing.T) {
	s, _ := leaseTestOpen(t, managedTestConfig())
	c := leaseTestClient(1)
	c.Hostname = "Owner"
	bound := leaseTestBind(t, s, c)
	before, _ := s.DNS().Address(bound.Address)
	now := bound.ExpiresAt.Add(-time.Minute)
	s.now = func() time.Time { return now }
	c.Hostname = "OWNER.leases.test."
	if _, err := s.Acknowledge(context.Background(), c, bound.Address, Renewing); err != nil {
		t.Fatal(err)
	}
	after, _ := s.DNS().Address(bound.Address)
	if before.ClaimEpoch != after.ClaimEpoch || before.ClaimTicket != after.ClaimTicket {
		t.Fatal("equivalent spelling reset ownership")
	}
	c.Hostname = "different"
	if _, err := s.Acknowledge(context.Background(), c, bound.Address, Renewing); err != nil {
		t.Fatal(err)
	}
	after, _ = s.DNS().Address(bound.Address)
	if after.ClaimTicket <= before.ClaimTicket {
		t.Fatal("renamed owner retained previous claim order")
	}
}

func TestDNSClaimGateCancellationPublishesNothing(t *testing.T) {
	s, _ := leaseTestOpen(t, managedTestConfig())
	client := leaseTestClient(1)
	offered := leaseTestOffer(t, s, client, netip.Addr{})
	before := s.DNS()
	gate, err := lockDNSClaims(context.Background(), s.registryPath+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Acknowledge(ctx, client, offered.Address, Selecting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked claim ignored cancellation: %v", err)
	}
	if s.DNS() != before {
		t.Fatal("canceled claim published")
	}
}

func TestLeaseCacheDetectsIndependentSQLiteWriter(t *testing.T) {
	cfg := managedTestConfig()
	s, path := leaseTestOpen(t, cfg)
	leaseTestBind(t, s, leaseTestClient(1))
	// total_changes is connection-local; data_version must detect another SQLite
	// connection even when that writer deliberately ignores our owner flock.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("UPDATE leases SET hostname='independent'"); err != nil {
		t.Fatal(err)
	}
	rows := leaseTestList(t, s)
	if rows[0].Hostname != "independent" {
		t.Fatalf("independent write omitted: %+v", rows)
	}
	record, ok := s.DNS().Address(rows[0].Address)
	if !ok || record.Name != "independent.leases.test." {
		t.Fatalf("independent write not reconciled: %+v", record)
	}
}

func TestLeaseCacheExpiryAndReplacementPreservesClientIndex(t *testing.T) {
	ctx := context.Background()
	cfg := leaseTestConfig()
	s, _ := leaseTestOpen(t, cfg)
	c := leaseTestClient(1)
	c.ID = "stable"
	for i := 0; i < 50; i++ {
		first := leaseTestOffer(t, s, c, cfg.RangeStart)
		now := first.ExpiresAt
		s.now = func() time.Time { return now }
		replacement, err := s.Offer(ctx, c, cfg.RangeEnd)
		if err != nil || replacement.Address != cfg.RangeEnd {
			t.Fatalf("replacement failed: %+v %v", replacement, err)
		}
		again, err := s.Offer(ctx, c, cfg.RangeStart)
		if err != nil || again.Address != replacement.Address {
			t.Fatalf("client index lost after expired replacement: %+v %v", again, err)
		}
		if _, err := s.Acknowledge(ctx, c, replacement.Address, Selecting); err != nil {
			t.Fatal(err)
		}
		if err := s.Release(ctx, c, replacement.Address); err != nil {
			t.Fatal(err)
		}
	}
}
