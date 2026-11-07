package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

func anchorRuntimeLegacy(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate(t.Context(), db, migrations[:7], 7, time.Now); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func anchorRuntimePreservedRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows := anchorDBRows(t, db, false)
	removed := 0
	rows["meta"] = slices.DeleteFunc(rows["meta"], func(raw string) bool {
		var pair []string
		if err := json.Unmarshal([]byte(raw), &pair); err != nil || len(pair) != 2 {
			t.Fatal("invalid metadata fixture")
		}
		if pair[0] == anchorLocalKey {
			removed++
			return true
		}
		return false
	})
	if removed != 1 {
		t.Fatal("upgrade did not add exactly one private carrier")
	}
	return rows
}

func TestAnchorRuntimeSchema7UpgradePreservesOriginalRows(t *testing.T) {
	for _, dormant := range []bool{false, true} {
		t.Run(map[bool]string{false: "sentinel", true: "dormant"}[dormant], func(t *testing.T) {
			db, path := anchorRuntimeLegacy(t)
			if dormant {
				var cluster string
				if err := db.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key=?`, metaClusterID).Scan(&cluster); err != nil {
					t.Fatal(err)
				}
				owner, err := accepted.NewDormant(cluster, strings.Repeat("a", 32), time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				rows, err := accepted.EncodeRows(owner)
				if err != nil || len(rows) != 1 {
					t.Fatal("invalid dormant fixture", err)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE replication_sections SET data=? WHERE section=? AND key=?`, string(rows[0].Data), accepted.Section, accepted.RootKey); err != nil {
					t.Fatal(err)
				}
			}
			before := anchorDBRows(t, db, false)
			if err := migrate(t.Context(), db, migrations, store.SchemaVersion, time.Now); err != nil {
				t.Fatal(err)
			}
			requireAnchorSchema(t, db, "8")
			local, err := loadAnchorLocal(t.Context(), db)
			if err != nil || local == nil || local.record.Phase != "awaiting" || local.record.Owner != (accepted.Version{}) {
				t.Fatal("physical migration granted or invented history", err)
			}
			if !reflect.DeepEqual(before, anchorRuntimePreservedRows(t, db)) {
				t.Fatal("schema8 changed original persisted rows")
			}
			original := local.raw
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, path)
			restored, err := loadAnchorLocal(t.Context(), s.r)
			if err != nil || restored.raw != original {
				t.Fatal("Store.Open replaced original carrier", err)
			}
		})
	}
}

func TestAnchorRuntimeMigrationRollbackAndActualSchema7Downgrade(t *testing.T) {
	for _, stage := range []string{"post-carrier-failure", "canceled", "schema7-reader"} {
		t.Run(stage, func(t *testing.T) {
			db, _ := anchorRuntimeLegacy(t)
			if stage == "schema7-reader" {
				if err := migrate(t.Context(), db, migrations, 8, time.Now); err != nil {
					t.Fatal(err)
				}
			}
			before := anchorDBRows(t, db, false)
			var err error
			switch stage {
			case "post-carrier-failure":
				refused := errors.New("fixture: physical completion refused")
				steps := slices.Clone(migrations)
				steps[7].up = func(ctx context.Context, tx *sql.Tx) error {
					if err := migrateAnchorLocal(ctx, tx); err != nil {
						return err
					}
					return refused
				}
				err = migrate(t.Context(), db, steps, 8, time.Now)
				if !errors.Is(err, refused) {
					t.Fatal("carrier failure changed category", err)
				}
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				err = migrate(ctx, db, migrations, 8, time.Now)
				if !errors.Is(err, context.Canceled) {
					t.Fatal("canceled upgrade proceeded", err)
				}
			case "schema7-reader":
				err = migrate(t.Context(), db, migrations[:7], 7, time.Now)
				if !errors.Is(err, ErrSchemaTooNew) {
					t.Fatal("actual schema7 target accepted schema8", err)
				}
			}
			want := "7"
			if stage == "schema7-reader" {
				want = "8"
			}
			requireAnchorSchema(t, db, want)
			if !reflect.DeepEqual(before, anchorDBRows(t, db, false)) {
				t.Fatal("refused migration changed original rows")
			}
		})
	}
}

func TestAnchorRuntimeCurrentSchemaMissingCarrierNeverRepairs(t *testing.T) {
	db, path := anchorRuntimeLegacy(t)
	if err := migrate(t.Context(), db, migrations, 8, time.Now); err != nil {
		t.Fatal(err)
	}
	result, err := db.ExecContext(t.Context(), `DELETE FROM meta WHERE key=?`, anchorLocalKey)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatal("fixture did not remove exact carrier", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := s.BindAnchorLifecycle(WithWriterToken(ctx, writer)); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("current-schema absence repaired or admitted", err)
	}
	var count int
	if err := s.r.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM meta WHERE key=?`, anchorLocalKey).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed bind recreated private history", err)
	}
}
