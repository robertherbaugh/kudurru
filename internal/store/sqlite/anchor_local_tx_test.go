package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

func TestAnchorLocalMigrationFreshChainAndExactOldOwner(t *testing.T) {
	t.Run("fresh-chain", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		db, err := sql.Open("sqlite", dsn(path, time.Second, true))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		steps := append(slices.Clone(migrations), migration{version: 8, name: "fixture unmounted local-deny prerequisite", up: migrateAnchorLocal})
		if err := migrate(t.Context(), db, steps, 8, time.Now); err != nil {
			t.Fatal(err)
		}
		local, err := loadAnchorLocal(t.Context(), db)
		if err != nil || local == nil || local.record.Phase != "awaiting" {
			t.Fatal("fresh chain lacks exact sentinel provenance", err)
		}
	})
	t.Run("dormant-bytes", func(t *testing.T) {
		s := openTemp(t)
		anchorTestDormant(t, s)
		before := imageOf(t, s)
		anchorLocalTestPhysical(t, s)
		if !before.Generation().Same(imageOf(t, s).Generation()) {
			t.Fatal("physical carrier migration changed accepted bytes")
		}
		source := anchorTestBind(t, s)
		if source.cursor.Load().local.record.Phase != "awaiting" {
			t.Fatal("Bind initialized without original startup command")
		}
		ctx := anchorTestContext(t.Context(), t, s)
		if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error { return tx.InitializeAnchorLocal(ctx) }); err != nil {
			t.Fatal(err)
		}
		if !before.Generation().Same(imageOf(t, s).Generation()) {
			t.Fatal("unchanged-Dormant local initialization advanced canonical image")
		}
		local := anchorLocalTestRead(t, s)
		if local.record.Phase != "dormant" || local.record.Owner != source.cursor.Load().owner.Version() {
			t.Fatal("unchanged-Dormant local binding absent")
		}
	})
}

func TestAnchorLocalMigrationRefusesManagedMissingUnknownAndRepeat(t *testing.T) {
	for _, kind := range []string{"missing-owner", "orphan-owner", "managed7", "unknown-local", "repeat"} {
		t.Run(kind, func(t *testing.T) {
			s := openTemp(t)
			switch kind {
			case "missing-owner":
				if _, err := s.w.ExecContext(t.Context(), `DELETE FROM replication_sections WHERE section=? AND key=?`, accepted.Section, accepted.RootKey); err != nil {
					t.Fatal(err)
				}
			case "orphan-owner":
				if _, err := s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,'trustanchors/v99/root','{}')`, accepted.Section); err != nil {
					t.Fatal(err)
				}
			case "managed7":
				dormant := anchorTestDormant(t, s)
				proof := anchorTestBootstrap(t, s, dormant)
				rows, err := accepted.EncodeRows(proof.next)
				if err != nil {
					t.Fatal(err)
				}
				// Controlled physical legacy fixture, never a public-prefix bypass.
				for _, row := range rows {
					if _, err := s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?) ON CONFLICT(section,key) DO UPDATE SET data=excluded.data`, accepted.Section, row.Key, string(row.Data)); err != nil {
						t.Fatal(err)
					}
				}
			case "unknown-local":
				if _, err := s.w.ExecContext(t.Context(), `INSERT INTO meta(key,value) VALUES ('trustanchors-local/v99/state','{}')`); err != nil {
					t.Fatal(err)
				}
			case "repeat":
				anchorLocalTestPhysical(t, s)
			}
			before, err := readMeta(t.Context(), s.r)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.w.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := migrateAnchorLocal(t.Context(), tx); err == nil {
				t.Fatal("invalid physical history repaired")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			after, err := readMeta(t.Context(), s.r)
			if err != nil {
				t.Fatal(err)
			}
			if len(before) != len(after) {
				t.Fatal("refused migration changed metadata")
			}
			for k, v := range before {
				if after[k] != v {
					t.Fatal("refused migration changed original bytes")
				}
			}
		})
	}
}

func TestAnchorLocalStartupRequiresTypedBoundaryEvenUnchanged(t *testing.T) {
	for _, kind := range []string{"missing-typed", "cancel-final", "refuse-final", "typed-noop"} {
		t.Run(kind, func(t *testing.T) {
			s := openTemp(t)
			anchorLocalTestPhysical(t, s)
			source := anchorTestBind(t, s)
			dormant, err := accepted.NewDormant(s.Authority().Term.ClusterID, strings.Repeat("b", 32), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			before := imageOf(t, s).Generation()
			local := anchorLocalTestRead(t, s)
			ctx, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
			defer cancel()
			refused := errors.New("fixture: startup final refusal")
			err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if err := anchorTestStage(ctx, tx, dormant); err != nil {
					return err
				}
				if kind == "missing-typed" {
					return nil
				}
				if err := tx.InitializeAnchorLocal(ctx); err != nil {
					return err
				}
				if kind == "cancel-final" {
					cancel()
				}
				if kind == "refuse-final" {
					return tx.RequireCommitCheck(func(context.Context) error { return refused })
				}
				return nil
			})
			if kind == "typed-noop" {
				if err != nil {
					t.Fatal(err)
				}
				acceptedCursor := source.cursor.Load()
				version := anchorLocalTestRead(t, s)
				if err := s.Command(anchorTestContext(t.Context(), t, s), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error { return tx.InitializeAnchorLocal(ctx) }); err != nil {
					t.Fatal(err)
				}
				if source.cursor.Load() != acceptedCursor || !sameAnchorLocal(version, anchorLocalTestRead(t, s)) {
					t.Fatal("unchanged local initialization reset identity")
				}
			} else {
				if err == nil {
					t.Fatal("refused startup committed")
				}
				if !before.Same(imageOf(t, s).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) || !source.cursor.Load().awaiting {
					t.Fatal("startup rollback split owner/local/source")
				}
			}
		})
	}
}

func TestAnchorLocalBootstrapSameTransactionFaultAndFinalRefusal(t *testing.T) {
	for _, kind := range []string{"local-SQL-fault", "final-refusal", "success"} {
		t.Run(kind, func(t *testing.T) {
			s := openTemp(t)
			source, dormant := anchorLocalTestDormant(t, s)
			before := imageOf(t, s).Generation()
			local := anchorLocalTestRead(t, s)
			if kind == "local-SQL-fault" {
				if _, err := s.w.ExecContext(t.Context(), `CREATE TEMP TRIGGER anchor_local_abort BEFORE UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN SELECT RAISE(ABORT,'fixture local write refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ctx := anchorTestContext(t.Context(), t, s)
			update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
			if err != nil {
				t.Fatal(err)
			}
			proof := anchorTestBootstrap(t, s, dormant)
			receipt, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
				if err := anchorTestStage(ctx, tx, proof.next); err != nil {
					return err
				}
				if kind == "final-refusal" {
					return tx.RequireCommitCheck(func(context.Context) error { return errors.New("fixture: proof refused") })
				}
				return tx.RequireCommitCheck(func(context.Context) error { return proof.observation.CheckLifetime(proof.clock.Sample()) })
			})
			if kind == "success" {
				if err != nil || receipt.Outcome() != AnchorCommitted {
					t.Fatal("atomic bootstrap failed", err)
				}
				next := anchorLocalTestRead(t, s)
				if next.record.Owner != proof.next.Version() || next.record.RetiredThrough != proof.next.Version().Generation-1 || source.cursor.Load().local.raw != next.raw {
					t.Fatal("committed bootstrap local/source split")
				}
			} else {
				if err == nil || receipt.Outcome() != AnchorRolledBack {
					t.Fatal("atomic failure misclassified", err)
				}
				if !before.Same(imageOf(t, s).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
					t.Fatal("staged canonical rows escaped local rollback")
				}
			}
		})
	}
}

func TestAnchorLocalObjectBackupRestoreCannotResetCarrier(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	original := source.cursor.Load()
	local := anchorLocalTestRead(t, s)
	var snapshot bytes.Buffer
	if err := s.Snapshot(t.Context(), &snapshot, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.String(), anchorLocalPrefix) || strings.Contains(snapshot.String(), accepted.Prefix) {
		t.Fatal("object backup disclosed private lifecycle history")
	}
	if err := s.Restore(anchorTestContext(t.Context(), t, s), &snapshot); err != nil {
		t.Fatal(err)
	}
	if source.cursor.Load() != original || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
		t.Fatal("object restore reset lifecycle")
	}
}

func TestAnchorLocalClosedDatabaseRetainsExactCarrier(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	version := source.cursor.Load().owner.Version()
	local := anchorLocalTestRead(t, s)
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path, time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	restored, err := loadAnchorLocal(t.Context(), db)
	if err != nil || !sameAnchorLocal(local, restored) {
		t.Fatal("closed SQLite lost exact local bytes", err)
	}
	rows, err := loadAnchorRows(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(rows, local.record.Cluster)
	if err != nil || owner.Version() != version {
		t.Fatal("closed SQLite split accepted owner", err)
	}
	// Full Store.Open(schema8) + original restart/follower tombstone acceptance
	// is still REQUIRED after parent mounts schema8, startup and actual callers.
}
