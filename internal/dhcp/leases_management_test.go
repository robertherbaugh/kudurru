package dhcp

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
)

func managedTestConfig() Config {
	c := leaseTestConfig()
	c.Management, c.RegisterDNS, c.DomainName = "api", true, "leases.test"
	return c
}

func TestLeaseManagementPersistenceAndOwnership(t *testing.T) {
	ctx := context.Background()
	cfg := managedTestConfig()
	s, path := leaseTestOpen(t, cfg)
	state := s.Configuration()
	if state.Revision != 1 {
		t.Fatalf("initial revision: %d", state.Revision)
	}
	// All caller-owned slices must be detached on both input and output.
	cfg.DNSServers[0] = netip.MustParseAddr("203.0.113.9")
	state.Config.DNSServers[0] = netip.MustParseAddr("203.0.113.8")
	if s.Configuration().Config.DNSServers[0] == state.Config.DNSServers[0] || s.Configuration().Config.DNSServers[0] == cfg.DNSServers[0] {
		t.Fatal("configuration aliases caller memory")
	}
	opts := optionsFor(s.Configuration().Config)
	opts.DomainName, opts.LeaseDuration = "managed.test", time.Minute
	if err := s.UpdateOptions(ctx, 1, opts); err != nil {
		t.Fatal(err)
	}
	r := Reservation{MAC: leaseTestClient(1).MAC, IP: netip.MustParseAddr("192.0.2.90"), Hostname: "printer"}
	if err := s.PutReservation(ctx, 2, r); err != nil {
		t.Fatal(err)
	}
	wanted := s.Configuration()
	copyState := s.Configuration()
	copyState.Config.Reservations[0].Hostname = "mutated"
	if s.Configuration().Config.Reservations[0].Hostname != "printer" {
		t.Fatal("reservation aliases snapshot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = managedTestConfig()
	cfg.DomainName = "ignored-seed.test"
	reopened, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Configuration(); !reflect.DeepEqual(got, wanted) {
		t.Fatalf("persisted override lost: %+v want %+v", got, wanted)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Management = "file"
	reopened, err = OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Configuration(); got.Config.DomainName != cfg.DomainName || len(got.Config.Reservations) != 0 {
		t.Fatalf("file scope loaded API override: %+v", got)
	}
	if err := reopened.UpdateOptions(ctx, 1, opts); !errors.Is(err, ErrManagedByFile) {
		t.Fatalf("file mutation: %v", err)
	}
}

func TestLeaseManagementConcurrentRevision(t *testing.T) {
	s, _ := leaseTestOpen(t, managedTestConfig())
	opts := optionsFor(s.Configuration().Config)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- s.UpdateOptions(context.Background(), 1, opts) })
	}
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRevisionConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 7 || s.Configuration().Revision != 2 {
		t.Fatalf("revision race: %d successes, %d conflicts", successes, conflicts)
	}
}

func TestLeaseManagementRejectsAllLiveStateConflicts(t *testing.T) {
	for _, state := range []string{"offered", "bound", "declined"} {
		t.Run(state, func(t *testing.T) {
			cfg := managedTestConfig()
			s, _ := leaseTestOpen(t, cfg)
			c := leaseTestClient(1)
			l := leaseTestOffer(t, s, c, netip.Addr{})
			if state != "offered" {
				l = leaseTestBind(t, s, c)
			}
			if state == "declined" {
				if err := s.Decline(context.Background(), c, l.Address); err != nil {
					t.Fatal(err)
				}
				l = leaseTestList(t, s)[0]
			}
			before, snapshot := s.Configuration(), s.DNS()
			opts := optionsFor(before.Config)
			opts.RangeStart = l.Address.Next()
			if err := s.UpdateOptions(context.Background(), before.Revision, opts); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("pool invalidation: %v", err)
			}
			if err := s.PutReservation(context.Background(), before.Revision, Reservation{MAC: leaseTestClient(2).MAC, IP: l.Address}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("reservation steals address: %v", err)
			}
			if err := s.PutReservation(context.Background(), before.Revision, Reservation{MAC: c.MAC, IP: l.Address.Next()}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("reservation moves owner: %v", err)
			}
			if !reflect.DeepEqual(before, s.Configuration()) || s.DNS() != snapshot {
				t.Fatal("rejected mutation published state")
			}
			s.now = func() time.Time { return l.ExpiresAt }
			if err := s.UpdateOptions(context.Background(), before.Revision, opts); err != nil {
				t.Fatalf("expired conflict blocks update: %v", err)
			}
		})
	}
}

func TestLeaseManagementReservationDeletionProtectsOutsidePool(t *testing.T) {
	cfg := managedTestConfig()
	r := Reservation{MAC: leaseTestClient(1).MAC, IP: netip.MustParseAddr("192.0.2.90")}
	cfg.Reservations = []Reservation{r}
	s, _ := leaseTestOpen(t, cfg)
	l := leaseTestBind(t, s, leaseTestClient(1))
	if err := s.DeleteReservation(context.Background(), 1, r.MAC); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted live reservation: %v", err)
	}
	if err := s.Release(context.Background(), leaseTestClient(1), l.Address); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteReservation(context.Background(), 1, r.MAC); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseManagementPersistenceFailureDoesNotPublish(t *testing.T) {
	for _, fault := range []string{"write", "commit"} {
		t.Run(fault, func(t *testing.T) {
			s, path := leaseTestOpen(t, managedTestConfig())
			leaseTestBind(t, s, leaseTestClient(1))
			before, snapshot := s.Configuration(), s.DNS()
			if fault == "write" {
				leaseTestSQL(t, s, `CREATE TRIGGER fail_settings BEFORE UPDATE ON lease_settings BEGIN SELECT RAISE(ABORT, 'injected'); END`)
			} else {
				leaseTestSQL(t, s, `CREATE TABLE fault_parent(id INTEGER PRIMARY KEY); CREATE TABLE fault_child(parent INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_settings AFTER UPDATE ON lease_settings BEGIN INSERT INTO fault_child VALUES(1); END`)
			}
			opts := optionsFor(before.Config)
			opts.DomainName = "failed.test"
			if err := s.UpdateOptions(context.Background(), before.Revision, opts); err == nil {
				t.Fatal("failed write reported success")
			}
			if !reflect.DeepEqual(before, s.Configuration()) || s.DNS() != snapshot {
				t.Fatal("failed transaction published configuration or DNS")
			}
			leaseTestSQL(t, s, "DROP TRIGGER fail_settings")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenLeases(path, managedTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			if !reflect.DeepEqual(before, reopened.Configuration()) {
				t.Fatal("failed settings persisted after restart")
			}
		})
	}
}

func TestLeaseManagementMigrationAndFailedFirstSeed(t *testing.T) {
	cfg := leaseTestConfig()
	s, path := leaseTestOpen(t, cfg)
	original := leaseTestBind(t, s, leaseTestClient(1))
	quarantined := leaseTestBind(t, s, leaseTestClient(2))
	if err := s.Decline(context.Background(), leaseTestClient(2), quarantined.Address); err != nil {
		t.Fatal(err)
	}
	want := leaseTestList(t, s)
	// Represent a pre-management v1 DB with its original lease schema.
	leaseTestSQL(t, s, "DROP TABLE lease_settings; PRAGMA user_version=1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.Management = "api"
	bad.RangeStart = original.Address.Next()
	if reopened, err := OpenLeases(path, bad); err == nil {
		_ = reopened.Close()
		t.Fatal("bad API seed accepted")
	}
	cfg.Management = "api"
	reopened, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatalf("corrected seed blocked by failed migration: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := leaseTestList(t, reopened); !reflect.DeepEqual(got, want) {
		t.Fatalf("migration changed leases: %+v want %+v", got, want)
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != leaseSchema {
		t.Fatalf("schema: %d %v", version, err)
	}
}

func TestLeaseRelayGatewayExcluded(t *testing.T) {
	cfg := managedTestConfig()
	cfg.TrustedRelays = []TrustedRelay{{SourceIP: netip.MustParseAddr("198.51.100.1"), GatewayIP: cfg.RangeStart}}
	s, _ := leaseTestOpen(t, cfg)
	if s.allowed(cfg.RangeStart, leaseTestClient(1).MAC) {
		t.Fatal("gateway allowed")
	}
	l := leaseTestOffer(t, s, leaseTestClient(1), cfg.RangeStart)
	if l.Address == cfg.RangeStart {
		t.Fatal("gateway allocated")
	}
	if err := s.PutReservation(context.Background(), 1, Reservation{MAC: leaseTestClient(2).MAC, IP: cfg.RangeStart}); err == nil {
		t.Fatal("gateway reservation accepted")
	}
}
