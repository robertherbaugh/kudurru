package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
)

func enrollmentSchema8(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate(t.Context(), db, migrations[:8], 8, time.Now); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func TestEnrollmentSchema9MigrationPreservesRowsAndRefusesDowngrade(t *testing.T) {
	db, path := enrollmentSchema8(t)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES('membership','foreign-owner','{"kept":true}')`); err != nil {
		t.Fatal(err)
	}
	before := anchorDBRows(t, db, false)
	if err := migrate(t.Context(), db, migrations, 9, time.Now); err != nil {
		t.Fatal(err)
	}
	requireAnchorSchema(t, db, "9")
	after := anchorDBRows(t, db, false)
	removed := 0
	after["sections"] = slices.DeleteFunc(after["sections"], func(raw string) bool {
		var row []string
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		if len(row) == 3 && row[0] == accepted.Section && row[1] == accepted.RootKey {
			if row[2] != accepted.AwaitingOwnerJSON {
				t.Fatal("migration granted membership")
			}
			removed++
			return true
		}
		return false
	})
	if removed != 1 || !reflect.DeepEqual(before, after) {
		t.Fatal("schema9 rewrote preexisting rows")
	}
	if err := migrate(t.Context(), db, migrations[:8], 8, time.Now); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatal("schema8 binary accepted schema9", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	var raw string
	if err := s.r.QueryRowContext(t.Context(), `SELECT data FROM replication_sections WHERE section=? AND key=?`, accepted.Section, accepted.RootKey).Scan(&raw); err != nil || raw != accepted.AwaitingOwnerJSON {
		t.Fatal("reopen changed physical sentinel", err)
	}
}

func TestEnrollmentMigrationRefusesPreexistingRowsAndRollsBackSQLFailure(t *testing.T) {
	for _, mode := range []string{"existing-root", "orphan", "sql1811"} {
		t.Run(mode, func(t *testing.T) {
			db, _ := enrollmentSchema8(t)
			if mode == "sql1811" {
				if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER refuse_enrollment BEFORE INSERT ON replication_sections WHEN NEW.section='membership' BEGIN SELECT RAISE(ABORT,'enrollment migration refusal'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				key := accepted.RootKey
				if mode == "orphan" {
					key = accepted.Prefix + "unknown"
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES(?,?,?)`, accepted.Section, key, `{}`); err != nil {
					t.Fatal(err)
				}
			}
			before := anchorDBRows(t, db, false)
			err := migrate(t.Context(), db, migrations, 9, time.Now)
			if mode == "sql1811" {
				var target interface{ Code() int }
				if !errors.As(err, &target) || target.Code() != 1811 {
					t.Fatal("did not reach actual migration SQL1811", err)
				}
			} else if !errors.Is(err, accepted.ErrOwner) {
				t.Fatal("migration adopted existing owner", err)
			}
			requireAnchorSchema(t, db, "8")
			if !reflect.DeepEqual(before, anchorDBRows(t, db, false)) {
				t.Fatal("failed migration changed historical bytes")
			}
		})
	}
}
