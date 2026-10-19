package dhcp

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func leaseTestSQL(t *testing.T, s *LeaseStore, query string) {
	t.Helper()
	if _, err := s.db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestLeasePersistenceFailures(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	ctx := context.Background()
	c := leaseTestClient(1)
	leaseTestSQL(t, s, `CREATE TRIGGER fail_write BEFORE INSERT ON leases BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`)
	if l, err := s.Offer(ctx, c, netip.Addr{}); err == nil || l.Address.IsValid() {
		t.Fatalf("failed persistence returned an offer: %+v %v", l, err)
	}
	if len(leaseTestList(t, s)) != 0 {
		t.Fatal("failed offer allocated an address")
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_write")
	offer := leaseTestOffer(t, s, c, netip.Addr{})
	leaseTestSQL(t, s, `CREATE TRIGGER fail_update BEFORE UPDATE ON leases BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`)
	if l, err := s.Acknowledge(ctx, c, offer.Address, Selecting); err == nil || l.Address.IsValid() {
		t.Fatalf("failed persistence returned an ACK: %+v %v", l, err)
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != offer {
		t.Fatalf("failed ACK changed offer: %+v", ls)
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_update")
	bound, err := s.Acknowledge(ctx, c, offer.Address, Selecting)
	if err != nil {
		t.Fatal(err)
	}
	leaseTestSQL(t, s, `CREATE TRIGGER fail_update BEFORE UPDATE ON leases BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`)
	if err := s.Decline(ctx, c, bound.Address); err == nil {
		t.Fatal("failed decline reported success")
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != leasePersistent(bound) {
		t.Fatalf("failed decline changed binding: %+v", ls)
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_update")
	leaseTestSQL(t, s, `CREATE TRIGGER fail_delete BEFORE DELETE ON leases BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`)
	if err := s.Release(ctx, c, bound.Address); err == nil {
		t.Fatal("failed release reported success")
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != leasePersistent(bound) {
		t.Fatalf("failed release changed binding: %+v", ls)
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_delete")
	if err := s.Release(ctx, c, bound.Address); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseCommitFailureRollsBackAndRecovers(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	ctx := context.Background()
	leaseTestSQL(t, s, `
CREATE TABLE fault_parent(id INTEGER PRIMARY KEY);
CREATE TABLE fault_child(parent INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER fail_commit AFTER INSERT ON leases BEGIN INSERT INTO fault_child(parent) VALUES (1); END;
`)
	// The INSERT succeeds; the deferred foreign key fails only at COMMIT.
	if l, err := s.Offer(ctx, leaseTestClient(1), netip.Addr{}); err == nil || l.Address.IsValid() {
		t.Fatalf("failed COMMIT returned an offer: %+v %v", l, err)
	}
	if ls := leaseTestList(t, s); len(ls) != 0 {
		t.Fatalf("failed COMMIT retained allocation: %+v", ls)
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_commit")
	offer := leaseTestOffer(t, s, leaseTestClient(1), netip.Addr{})
	leaseTestSQL(t, s, `CREATE TRIGGER fail_commit AFTER UPDATE ON leases BEGIN INSERT INTO fault_child(parent) VALUES (1); END`)
	if l, err := s.Acknowledge(ctx, leaseTestClient(1), offer.Address, Selecting); err == nil || l.Address.IsValid() {
		t.Fatalf("failed COMMIT returned an ACK: %+v %v", l, err)
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != offer {
		t.Fatalf("failed ACK COMMIT changed offer: %+v", ls)
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_commit")
	if _, err := s.Acknowledge(ctx, leaseTestClient(1), offer.Address, Selecting); err != nil {
		t.Fatalf("store failed to recover after commit failure: %v", err)
	}
}

func TestLeaseExpiryFailureDoesNotReuse(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.RangeEnd = cfg.RangeStart
	s, _ := leaseTestOpen(t, cfg)
	l := leaseTestBind(t, s, leaseTestClient(1))
	s.now = func() time.Time { return l.ExpiresAt }
	leaseTestSQL(t, s, `CREATE TRIGGER fail_expire BEFORE DELETE ON leases BEGIN SELECT RAISE(ABORT, 'injected expiry failure'); END`)
	if l, err := s.Offer(context.Background(), leaseTestClient(2), cfg.RangeStart); err == nil || l.Address.IsValid() {
		t.Fatalf("failed expiry reused an address: %+v %v", l, err)
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_expire")
	if reused := leaseTestOffer(t, s, leaseTestClient(2), cfg.RangeStart); reused.Address != cfg.RangeStart {
		t.Fatalf("address not reusable after persistence recovery: %+v", reused)
	}
}

func TestLeaseConfigChangesProtectLiveState(t *testing.T) {
	for _, state := range []string{"bound", "declined"} {
		for _, tc := range []struct {
			name string
			edit func(*Config, Lease)
		}{
			{"pool shrunk", func(c *Config, l Lease) { c.RangeStart = l.Address.Next() }},
			{"server changed", func(c *Config, l Lease) { c.ServerIP = l.Address }},
			{"router changed", func(c *Config, l Lease) { c.Router = l.Address }},
			{"reservation stolen", func(c *Config, l Lease) { c.Reservations = []Reservation{{MAC: leaseTestClient(2).MAC, IP: l.Address}} }},
		} {
			t.Run(state+"/"+tc.name, func(t *testing.T) {
				cfg := leaseTestConfig()
				s, path := leaseTestOpen(t, cfg)
				l := leaseTestBind(t, s, leaseTestClient(1))
				if state == "declined" {
					if err := s.Decline(context.Background(), leaseTestClient(1), l.Address); err != nil {
						t.Fatal(err)
					}
					l = leaseTestList(t, s)[0]
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				changed := cfg
				tc.edit(&changed, l)
				if reopened, err := OpenLeases(path, changed); err == nil {
					_ = reopened.Close()
					t.Fatal("incompatible live lease accepted")
				}
				// Failed startup neither drops state nor leaks the owner lock.
				s, err := OpenLeases(path, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != l {
					t.Fatalf("failed startup lost live state: %+v", ls)
				}
			})
		}
	}
}

func TestLeaseReservationRemovalAndCompatibleChanges(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.Reservations = []Reservation{{MAC: leaseTestClient(1).MAC, IP: netip.MustParseAddr("192.0.2.100")}}
	s, path := leaseTestOpen(t, cfg)
	l := leaseTestBind(t, s, leaseTestClient(1))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.Reservations = nil
	if reopened, err := OpenLeases(path, changed); err == nil {
		_ = reopened.Close()
		t.Fatal("removed live reservation outside pool was forgotten")
	}
	// Moving the reservation keeps the binding until the client returns.
	changed.Reservations = []Reservation{{MAC: l.MAC, IP: netip.MustParseAddr("192.0.2.101")}}
	s, err := OpenLeases(path, changed)
	if err != nil {
		t.Fatalf("moved reservation refused: %v", err)
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != l {
		t.Fatalf("moved reservation changed the binding: %+v", ls)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	changed = cfg
	changed.DomainName, changed.LeaseDuration = "new.example", time.Hour
	s, err = OpenLeases(path, changed)
	if err != nil {
		t.Fatal(err)
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0] != l {
		t.Fatalf("compatible change modified existing binding: %+v", ls)
	}
	// Once expired, old state no longer prevents changing the pool/reservation.
	leaseTestSQL(t, s, "UPDATE leases SET expires_at = 1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	changed.Reservations = nil
	s, err = OpenLeases(path, changed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if len(leaseTestList(t, s)) != 0 {
		t.Fatal("expired incompatible lease retained")
	}
}

func TestLeaseAcknowledgmentPreservesBoundExpiry(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		duration   time.Duration
		clockShift time.Duration
	}{
		{"shorter duration after restart", time.Minute, time.Hour},
		{"clock moved backward", 12 * time.Hour, -time.Hour},
	} {
		for _, mode := range []struct {
			name string
			mode RequestMode
		}{{"selecting", Selecting}, {"init-reboot", InitReboot}, {"renewing", Renewing}} {
			t.Run(scenario.name+"/"+mode.name, func(t *testing.T) {
				cfg := leaseTestConfig()
				cfg.RangeEnd = cfg.RangeStart
				s, path := leaseTestOpen(t, cfg)
				started := time.Now().UTC().Truncate(time.Second)
				now := started
				s.now = func() time.Time { return now }
				client := leaseTestClient(1)
				original := leaseTestBind(t, s, client)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}

				cfg.LeaseDuration = scenario.duration
				s, err := OpenLeases(path, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				now = started.Add(scenario.clockShift)
				s.now = func() time.Time { return now }
				acked, err := s.Acknowledge(context.Background(), client, original.Address, mode.mode)
				if err != nil || !acked.ExpiresAt.Equal(original.ExpiresAt) {
					t.Fatalf("ACK shortened existing binding: original=%+v ack=%+v err=%v", original, acked, err)
				}
				// Treat that ACK as lost: the client still relies on the original
				// expiration. Reopen again to check what actually reached disk.
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = OpenLeases(path, cfg)
				if err != nil {
					t.Fatal(err)
				}
				s.now = func() time.Time { return now }
				if leases := leaseTestList(t, s); len(leases) != 1 || !leases[0].ExpiresAt.Equal(original.ExpiresAt) {
					t.Fatalf("restart did not preserve original expiration: %+v", leases)
				}
				other := leaseTestClient(2)
				for _, checkpoint := range []time.Time{
					now.Add(cfg.LeaseDuration + time.Second),
					original.ExpiresAt.Add(-time.Second),
				} {
					now = checkpoint
					if lease, err := s.Offer(context.Background(), other, original.Address); !errors.Is(err, ErrNoAddress) || lease.Address.IsValid() {
						t.Fatalf("address reused before original client expiry at %s: lease=%+v err=%v", now, lease, err)
					}
				}
				now = original.ExpiresAt
				reused := leaseTestBind(t, s, other)
				if reused.Address != original.Address || !reused.ExpiresAt.Equal(now.Add(cfg.LeaseDuration)) {
					t.Fatalf("new lease did not use configured duration at original expiry: %+v", reused)
				}
			})
		}
	}
}

func TestLeaseDatabaseConstraintsAndVersion(t *testing.T) {
	cfg := leaseTestConfig()
	s, path := leaseTestOpen(t, cfg)
	l := leaseTestBind(t, s, leaseTestClient(1))
	// Exercise the database constraints independently of Go's serialization.
	for _, query := range []string{
		`INSERT INTO leases SELECT address, 'another-key', client_id, mac, hostname, expires_at, state FROM leases`,
		`INSERT INTO leases SELECT address+1, client_key, client_id, mac, hostname, expires_at, state FROM leases`,
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatal("SQLite uniqueness constraint missing")
		}
	}
	var journal string
	var syncMode, version, app int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode: %q %v", journal, err)
	}
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil || syncMode != 2 {
		t.Fatalf("synchronous mode: %v %v", syncMode, err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != leaseSchema {
		t.Fatalf("schema version: %v %v", version, err)
	}
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&app); err != nil || app != leaseAppID {
		t.Fatalf("application ID: %v %v", app, err)
	}
	leaseTestSQL(t, s, "PRAGMA user_version = 999")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenLeases(path, cfg); err == nil {
		_ = reopened.Close()
		t.Fatal("future schema accepted")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var address uint32
	if err := db.QueryRow("SELECT address FROM leases").Scan(&address); err != nil || address != addressNumber(l.Address) {
		t.Fatalf("future-schema refusal modified database: %v %v", address, err)
	}
}

func TestLeaseRefusesUnrelatedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE unrelated(secret TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenLeases(path, leaseTestConfig()); err == nil {
		_ = s.Close()
		t.Fatal("unrelated database accepted")
	}
}

func TestLeaseCrashRecoveryProcess(t *testing.T) {
	if path := os.Getenv("DHCP_LEASE_CRASH_TEST_PATH"); path != "" {
		s, err := OpenLeases(path, leaseTestConfig())
		if err != nil {
			t.Fatal(err)
		}
		leaseTestBind(t, s, leaseTestClient(1))
		l := leaseTestBind(t, s, leaseTestClient(2))
		if err := s.Decline(context.Background(), leaseTestClient(2), l.Address); err != nil {
			t.Fatal(err)
		}
		leaseTestOffer(t, s, leaseTestClient(3), netip.Addr{})
		// Deliberately skip Close, leaving committed state in the WAL and
		// relying on process exit to release the exclusive owner lock.
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeaseCrashRecoveryProcess$") // #nosec G204 G702 -- reruns this test executable
	cmd.Env = append(os.Environ(), "DHCP_LEASE_CRASH_TEST_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child lease writer: %v\n%s", err, output)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("crash did not leave a WAL: %v", err)
	}
	s, err := OpenLeases(path, leaseTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ls := leaseTestList(t, s)
	if len(ls) != 3 || ls[0].State != "bound" || ls[1].State != "declined" || ls[2].State != "offered" {
		t.Fatalf("WAL recovery lost lease states: %+v", ls)
	}
	if _, err := s.Offer(context.Background(), leaseTestClient(4), netip.Addr{}); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("recovered address allocated twice: %v", err)
	}
}
