//go:build unix

package dhcp

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestRetiredDatabaseRejectsLiveAllocationsAndReleasesLock(t *testing.T) {
	for _, state := range []string{"offered", "bound", "declined"} {
		t.Run(state, func(t *testing.T) {
			cfg := leaseTestConfig()
			s, path := leaseTestOpen(t, cfg)
			leaseTestBind(t, s, leaseTestClient(1))
			if _, err := s.db.Exec("UPDATE leases SET state = ?", state); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if owner, err := CheckRetiredDatabase(path); err == nil {
				_ = owner.Close()
				t.Fatal("retired a database with a live allocation")
			}
			s, err := OpenLeases(path, cfg)
			if err != nil {
				t.Fatalf("failed retirement retained ownership: %v", err)
			}
			defer func() { _ = s.Close() }()
		})
	}
}

func TestRetiredDatabaseHoldsEmptyAndExpiredScopes(t *testing.T) {
	for _, expired := range []bool{false, true} {
		cfg := leaseTestConfig()
		s, path := leaseTestOpen(t, cfg)
		if expired {
			s.now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
			leaseTestBind(t, s, leaseTestClient(1))
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		owner, err := CheckRetiredDatabase(path)
		if err != nil {
			t.Fatal(err)
		}
		if other, err := OpenLeases(path, cfg); err == nil {
			_ = other.Close()
			t.Fatal("retired database could acquire a second owner")
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = OpenLeases(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestRetiredDatabaseRejectsUnknownSchema(t *testing.T) {
	for _, version := range []int{0, leaseSchema + 1} {
		s, path := leaseTestOpen(t, leaseTestConfig())
		_ = s.Close()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		pragma := "PRAGMA user_version = 0"
		if version > 0 {
			pragma = "PRAGMA user_version = 999"
		}
		if _, err := db.Exec(pragma); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if owner, err := CheckRetiredDatabase(path); err == nil {
			_ = owner.Close()
			t.Fatal("unknown database version retired")
		}
	}
}

func TestServiceDirectoryExclusiveOwnership(t *testing.T) {
	dir := t.TempDir()
	owner, err := LockServiceDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := LockServiceDirectory(filepath.Join(dir, ".")); err == nil {
		_ = second.Close()
		t.Fatal("second directory owner accepted")
	}
	_ = owner.Close()
	owner, err = LockServiceDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = owner.Close()
}

func TestRetiredUninitializedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		allowed      bool
	}{
		{"empty", "", true},
		{"unrelated table", "CREATE TABLE unrelated (id INTEGER)", false},
		{"unrelated view", "CREATE VIEW unrelated AS SELECT 1", false},
		{"reserved prefix lookalike", "CREATE TABLE sqlitex_data (id INTEGER)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dhcp.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Ping(); err != nil {
				t.Fatal(err)
			}
			if tc.schema != "" {
				if _, err := db.Exec(tc.schema); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			owner, err := CheckRetiredDatabase(path)
			if !tc.allowed {
				if err == nil {
					_ = owner.Close()
					t.Fatal("retired an unrelated database")
				}
				// Rejection releases ownership as well.
				lock, err := lockLeaseFile(path)
				if err != nil {
					t.Fatal(err)
				}
				_ = lock.Close()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if other, err := OpenLeases(path, leaseTestConfig()); err == nil {
				_ = other.Close()
				t.Fatal("retired empty database allowed a second owner")
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := OpenLeases(path, leaseTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			_ = store.Close()
		})
	}
}
