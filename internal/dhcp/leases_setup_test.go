package dhcp

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func setupLeaseMetadata(t *testing.T, path string) LeaseSettings {
	t.Helper()
	settings, exists, err := ReadLeaseSettings(context.Background(), path)
	if err != nil || !exists {
		t.Fatalf("read settings: exists=%v err=%v", exists, err)
	}
	return settings
}

func TestLeaseSetupApplyReplayAndRevision(t *testing.T) {
	cfg := managedTestConfig()
	s, path := leaseTestOpen(t, cfg)
	bound := leaseTestBind(t, s, leaseTestClient(1))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.DomainName, cfg.LeaseDuration = "staged.test", time.Minute
	s, err := OpenLeasesForSetup(path, cfg, "first", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if meta := setupLeaseMetadata(t, path); meta.Revision != 2 || meta.AppliedSetupToken != "first" || meta.Options.DomainName != cfg.DomainName {
		t.Fatalf("staged settings not committed: %+v", meta)
	}
	if got := leaseTestList(t, s); len(got) != 1 || got[0] != bound {
		t.Fatalf("staged shorter duration changed durable lease: %+v", got)
	}
	opts := optionsFor(s.Configuration().Config)
	opts.DomainName = "live.test"
	if err := s.UpdateOptions(context.Background(), 2, opts); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A listener failure can retry the same plan after live edits. Old expected
	// revision and seed values must never replay over those later edits.
	s, err = OpenLeasesForSetup(path, cfg, "first", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Configuration(); got.Revision != 3 || got.Config.DomainName != "live.test" {
		t.Fatalf("token replay replaced live settings: %+v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := setupLeaseMetadata(t, path)
	if unexpected, err := OpenLeasesForSetup(path, cfg, "second", 2); !errors.Is(err, ErrRevisionConflict) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		t.Fatalf("stale setup: %v", err)
	}
	if after := setupLeaseMetadata(t, path); !reflect.DeepEqual(before, after) {
		t.Fatal("stale startup changed metadata/token")
	}
}

func TestLeaseSetupLiveConflictsAndTransactionFailures(t *testing.T) {
	for _, fault := range []string{"offered", "bound", "declined", "write", "commit"} {
		t.Run(fault, func(t *testing.T) {
			cfg := managedTestConfig()
			s, path := leaseTestOpen(t, cfg)
			client := leaseTestClient(1)
			l := leaseTestOffer(t, s, client, cfg.RangeStart)
			if fault != "offered" {
				l = leaseTestBind(t, s, client)
			}
			if fault == "declined" {
				if err := s.Decline(context.Background(), client, l.Address); err != nil {
					t.Fatal(err)
				}
			}
			want := leaseTestList(t, s)
			before := setupLeaseMetadata(t, path)
			switch fault {
			case "write":
				leaseTestSQL(t, s, `CREATE TRIGGER fail_setup BEFORE UPDATE ON lease_settings BEGIN SELECT RAISE(ABORT, 'injected'); END`)
			case "commit":
				leaseTestSQL(t, s, `CREATE TABLE setup_parent(id INTEGER PRIMARY KEY); CREATE TABLE setup_child(parent INTEGER REFERENCES setup_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_setup AFTER UPDATE ON lease_settings BEGIN INSERT INTO setup_child VALUES(1); END`)
			default:
				cfg.RangeStart = l.Address.Next()
			}
			cfg.DomainName = "must-rollback.test"
			if err := CheckLeaseSetup(context.Background(), path, cfg, 1); fault != "write" && fault != "commit" && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("readonly preflight accepted live conflict: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if unexpected, err := OpenLeasesForSetup(path, cfg, "failed", 1); err == nil {
				_ = unexpected.Close()
				t.Fatal("unsafe/failed transaction returned allocator")
			}
			if after := setupLeaseMetadata(t, path); !reflect.DeepEqual(before, after) {
				t.Fatalf("failed startup changed settings: %+v want %+v", after, before)
			}
			s, err := OpenLeases(path, managedTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if got := leaseTestList(t, s); !reflect.DeepEqual(got, want) {
				t.Fatalf("failed startup changed allocations: %+v want %+v", got, want)
			}
		})
	}
}

func TestLeaseSetupReadOnlyAndV2Migration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "absent.db")
	cfg := managedTestConfig()
	if _, exists, err := ReadLeaseSettings(ctx, path); exists || err != nil {
		t.Fatalf("absent metadata: %v %v", exists, err)
	}
	if err := CheckLeaseSetup(ctx, path, cfg, 0); err != nil {
		t.Fatal(err)
	}
	if err := CheckLeaseSetup(ctx, path, cfg, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("absent revision: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := CheckLeaseSetup(canceled, path, cfg, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("absent canceled preflight: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 0 {
		t.Fatalf("readonly operations created files: %v %v", entries, err)
	}
	s, path := leaseTestOpen(t, cfg)
	bound := leaseTestBind(t, s, leaseTestClient(1))
	leaseTestSQL(t, s, "DROP TABLE lease_dns_claims; DROP TABLE lease_replica; DROP INDEX leases_expiry; ALTER TABLE lease_settings DROP COLUMN setup_token; PRAGMA user_version=2")
	if err := os.Chmod(path, 0o640); err != nil { //nolint:gosec // Deliberately verify metadata readers preserve existing permissions.
		t.Fatal(err)
	}
	if meta := setupLeaseMetadata(t, path); meta.Revision != 1 || meta.AppliedSetupToken != "" {
		t.Fatalf("v2 inspection: %+v", meta)
	}
	if err := CheckLeaseSetup(ctx, path, cfg, 1); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("readonly preflight migrated schema: %d %v", version, err)
	}
	if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != 0o640 {
		t.Fatalf("readonly inspection changed mode: %v %v", stat, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenLeasesForSetup(path, cfg, "migrated", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != leaseSchema {
		t.Fatalf("startup migration: %d %v", version, err)
	}
	if got := leaseTestList(t, s); len(got) != 1 || got[0] != bound {
		t.Fatalf("migration lost lease: %+v", got)
	}
	if meta := setupLeaseMetadata(t, path); meta.Revision != 2 || meta.AppliedSetupToken != "migrated" {
		t.Fatalf("migration/apply not atomic: %+v", meta)
	}
}

func TestLeaseSetupEmptyDatabaseRecoveryAndForeignSchema(t *testing.T) {
	ctx := context.Background()
	cfg := managedTestConfig()
	for _, kind := range []string{"failed-apply", "crash-empty", "foreign-table", "foreign-view", "foreign-sqlitex-table"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dhcp.db")
			switch kind {
			case "failed-apply":
				if unexpected, err := OpenLeasesForSetup(path, cfg, "stale", 1); !errors.Is(err, ErrRevisionConflict) {
					if unexpected != nil {
						_ = unexpected.Close()
					}
					t.Fatalf("missing DB accepted stale revision: %v", err)
				}
			case "crash-empty":
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				statement := "CREATE TABLE unrelated(id INTEGER)"
				switch kind {
				case "foreign-view":
					statement = "CREATE VIEW unrelated AS SELECT 1 AS id"
				case "foreign-sqlitex-table":
					statement = "CREATE TABLE sqlitex_unrelated(id INTEGER)"
				}
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			meta, exists, err := ReadLeaseSettings(ctx, path)
			if kind != "failed-apply" && kind != "crash-empty" {
				if err == nil {
					t.Fatal("unrelated database treated as uninitialized")
				}
				if err := CheckLeaseSetup(ctx, path, cfg, 0); err == nil {
					t.Fatal("preflight accepted unrelated database")
				}
				if unexpected, err := OpenLeasesForSetup(path, cfg, "foreign", 0); err == nil {
					_ = unexpected.Close()
					t.Fatal("startup overwrote unrelated schema")
				}
				return
			}
			if err != nil || !exists || meta.Revision != 0 || meta.AppliedSetupToken != "" {
				t.Fatalf("empty database cannot be repaired: %+v %v %v", meta, exists, err)
			}
			if err := CheckLeaseSetup(ctx, path, cfg, 1); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("empty DB bypassed stale preflight guard: %v", err)
			}
			if err := CheckLeaseSetup(ctx, path, cfg, 0); err != nil {
				t.Fatalf("empty DB failed fresh preflight: %v", err)
			}
			s, err := OpenLeasesForSetup(path, cfg, "repaired", 0)
			if err != nil {
				t.Fatalf("explicit revision0 recovery failed: %v", err)
			}
			defer func() { _ = s.Close() }()
			if got := setupLeaseMetadata(t, path); got.Revision != 1 || got.AppliedSetupToken != "repaired" {
				t.Fatalf("recovered settings missing: %+v", got)
			}
		})
	}
}
