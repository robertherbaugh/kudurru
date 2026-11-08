package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

func existingContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestOpenExistingPreservesDatabaseAndExclusiveOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path)
	mustApply(t, s, zone("original", "example."), store.ManagerAPI)
	info, authority, rv := s.Info(), s.Authority(), s.ResourceVersion()
	if _, err := OpenExisting(existingContext(t), path, testOptions()); !errors.Is(err, ErrLocked) {
		t.Fatal("offline opener bypassed actual process lock", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) // #nosec G304 -- exact test-owned database under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenExisting(existingContext(t), path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if s.Info() != info || !reflect.DeepEqual(s.Authority(), authority) || s.ResourceVersion() != rv {
		t.Fatal("existing open changed original identity or counters")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) // #nosec G304 -- exact test-owned database under t.TempDir
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing open/close mutated database bytes", err)
	}
}

func TestOpenExistingRefusesWithoutCreatingOrRepairing(t *testing.T) {
	for _, name := range []string{"missing", "empty", "directory", "symlink", "old-schema", "new-schema", "missing-schema", "missing-authority", "missing-cluster", "wrong-cluster", "missing-audit-boundary", "non-wal"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", FileName)
			if name == "missing" {
				if _, err := OpenExisting(existingContext(t), path, testOptions()); err == nil {
					t.Fatal("missing database initialized")
				}
				if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing parent created", err)
				}
				return
			}
			s := openAt(t, path)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			query := map[string]string{
				"old-schema":             "UPDATE meta SET value='7' WHERE key='schema_version'",
				"new-schema":             "UPDATE meta SET value='" + strconv.Itoa(store.SchemaVersion+1) + "' WHERE key='schema_version'",
				"missing-schema":         "DELETE FROM meta WHERE key='schema_version'",
				"missing-authority":      "DELETE FROM meta WHERE key='replication_authority'",
				"missing-cluster":        "DELETE FROM meta WHERE key='cluster_id'",
				"wrong-cluster":          "UPDATE meta SET value='foreign' WHERE key='cluster_id'",
				"missing-audit-boundary": "DELETE FROM meta WHERE key='audit_legacy_max_id'",
				"non-wal":                "PRAGMA journal_mode=DELETE",
			}[name]
			if query != "" {
				if _, err := db.ExecContext(t.Context(), query); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "empty":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = filepath.Dir(path)
			case "symlink":
				link := path + ".alias"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			var before []byte
			if name != "directory" {
				before, err = os.ReadFile(path) // #nosec G304 -- exact test-owned database or alias under t.TempDir
				if err != nil {
					t.Fatal(err)
				}
			}
			if opened, err := OpenExisting(existingContext(t), path, testOptions()); err == nil {
				_ = opened.Close()
				t.Fatal("incompatible database opened or repaired")
			}
			if name != "directory" {
				after, err := os.ReadFile(path) // #nosec G304 -- exact test-owned database or alias under t.TempDir
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("refusal changed original database bytes", err)
				}
			}
		})
	}
}

func TestOpenExistingRequiresOriginalFiniteLifetime(t *testing.T) {
	canceled, cancel := context.WithTimeout(t.Context(), time.Second)
	cancel()
	long, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	for _, ctx := range []context.Context{nil, context.Background(), canceled, long} {
		path := filepath.Join(t.TempDir(), FileName)
		if _, err := OpenExisting(ctx, path, testOptions()); err == nil {
			t.Fatal("invalid original lifetime admitted")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid lifetime created a database", err)
		}
	}
}
