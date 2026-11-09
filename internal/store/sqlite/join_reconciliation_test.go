package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestJoinIntentSQLCompletionAndDuplicateCapture(t *testing.T) {
	for _, mode := range []string{"success", "rollback1811", "commit787", "queue-deadline"} {
		t.Run(mode, func(t *testing.T) {
			_, f, inv, local := initialJoinPair(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var statements []string
			switch mode {
			case "rollback1811":
				statements = []string{`CREATE TEMP TRIGGER intent_abort BEFORE INSERT ON main.meta WHEN NEW.key='local_join_intent_v1' BEGIN SELECT RAISE(ABORT,'intent fixture refusal'); END`}
			case "commit787":
				statements = []string{`CREATE TEMP TABLE intent_parent(id INTEGER PRIMARY KEY)`, `CREATE TEMP TABLE intent_child(parent INTEGER REFERENCES intent_parent(id) DEFERRABLE INITIALLY DEFERRED)`, `CREATE TEMP TRIGGER intent_unknown AFTER INSERT ON main.meta WHEN NEW.key='local_join_intent_v1' BEGIN INSERT INTO intent_child(parent) VALUES (1); END`}
			case "queue-deadline":
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				f.wmu.Lock()
				defer f.wmu.Unlock()
			}
			for _, q := range statements {
				if _, err := f.w.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			before := f.Authority()
			request, err := f.CaptureInitialJoin(ctx, inv, local.Trust())
			if mode != "success" {
				if err == nil || request != nil {
					t.Fatal("unconfirmed intent returned dispatch authority", err)
				}
				if mode == "queue-deadline" {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("intent lock wait escaped original deadline", err)
					}
				} else {
					want := 1811
					if mode == "commit787" {
						want = 787
					}
					var code interface{ Code() int }
					if !errors.As(err, &code) || code.Code() != want {
						t.Fatal("actual SQL intent boundary not reached", err)
					}
					if mode == "commit787" && (!errors.Is(err, ErrJoinIntentUnknown) || !f.anchorBlocked.Load()) {
						t.Fatal("unknown local completion not fenced", err)
					}
				}
				if !reflect.DeepEqual(before, f.Authority()) {
					t.Fatal("intent failure changed authority")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var intent string
			if err := f.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaJoinIntent).Scan(&intent); err != nil {
				t.Fatal(err)
			}
			wire, err := inv.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(intent, wire) || !strings.Contains(intent, local.Trust().Fingerprint()) {
				t.Fatal("intent exposed wire or lost original CA")
			}
			if _, err := f.CaptureInitialJoin(ctx, inv, local.Trust()); err == nil {
				t.Fatal("second proposal capture reused durable intent")
			}
			if !reflect.DeepEqual(before, f.Authority()) {
				t.Fatal("intent write granted authority")
			}
		})
	}
}

func TestJoinReconciliationRequiresRetainedCutThroughInstallation(t *testing.T) {
	for _, mode := range []string{"restart", "missing-intent", "changed-intent", "replacement-ca", "canceled", "interposed", "rollback1811", "commit787"} {
		t.Run(mode, func(t *testing.T) {
			p, f, inv, local := initialJoinPair(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := f.CaptureInitialJoin(ctx, inv, local.Trust()); err != nil {
				t.Fatal(err)
			}
			wire, err := inv.Encode()
			if err != nil {
				t.Fatal(err)
			}
			observation, err := enrollment.ParseJoinObservation(wire)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "restart" {
				path := f.Path()
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				f, err = OpenExisting(ctx, path, testOptions(internalOptions))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				if _, err := f.BindAnchorLifecycle(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-intent" || mode == "changed-intent" {
				q := `DELETE FROM meta WHERE key='local_join_intent_v1'`
				if mode == "changed-intent" {
					q = `UPDATE meta SET value='{}' WHERE key='local_join_intent_v1'`
				}
				if _, err := f.w.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replacement-ca" {
				local, err = memberidentity.Create(filepath.Join(t.TempDir(), "replacement.json"), local.Trust().MemberID())
				if err != nil {
					t.Fatal(err)
				}
			}
			request, err := f.CaptureJoinReconciliation(ctx, observation, local.Trust())
			if mode == "missing-intent" || mode == "changed-intent" || mode == "replacement-ca" {
				if err == nil {
					t.Fatal("reconciliation repaired original intent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			image, err := p.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			validated, err := f.ValidateImage(ctx, *image)
			if err != nil {
				t.Fatal(err)
			}
			before, source := f.Authority(), f.anchors.Load()
			cursor := source.cursor.Load()
			switch mode {
			case "canceled":
				cancel()
			case "interposed":
				if _, err := f.w.ExecContext(ctx, `UPDATE meta SET value='{}' WHERE key='local_join_intent_v1'`); err != nil {
					t.Fatal(err)
				}
			case "rollback1811":
				if _, err := f.w.ExecContext(ctx, `CREATE TEMP TRIGGER reconcile_abort BEFORE INSERT ON main.replication_sections BEGIN SELECT RAISE(ABORT,'reconcile fixture refusal'); END`); err != nil {
					t.Fatal(err)
				}
			case "commit787":
				for _, q := range []string{`CREATE TEMP TABLE reconcile_parent(id INTEGER PRIMARY KEY)`, `CREATE TEMP TABLE reconcile_child(parent INTEGER REFERENCES reconcile_parent(id) DEFERRABLE INITIALLY DEFERRED)`, `CREATE TEMP TRIGGER reconcile_unknown AFTER INSERT ON main.replication_sections BEGIN INSERT INTO reconcile_child(parent) VALUES (1); END`} {
					if _, err := f.w.ExecContext(ctx, q); err != nil {
						t.Fatal(err)
					}
				}
			}
			copied := *request
			_, err = f.InstallJoinedReplica(request, validated)
			if mode == "restart" {
				if err != nil || f.Authority().Mode != replication.Follower || !f.Authority().Installed.Same(image.Generation()) {
					t.Fatal("retained reconciliation failed", err)
				}
				if _, err := f.CaptureJoinReconciliation(ctx, observation, local.Trust()); err == nil {
					t.Fatal("installed follower admitted first-image reconciliation")
				}
			} else {
				if err == nil || !reflect.DeepEqual(before, f.Authority()) || source.cursor.Load() != cursor {
					t.Fatal("failed reconciliation changed local state", err)
				}
				if mode == "rollback1811" || mode == "commit787" {
					want := 1811
					if mode == "commit787" {
						want = 787
					}
					var code interface{ Code() int }
					if !errors.As(err, &code) || code.Code() != want {
						t.Fatal("actual install SQL boundary not reached", err)
					}
				}
			}
			if _, err := f.InstallJoinedReplica(&copied, validated); err == nil {
				t.Fatal("copy replayed consumed observation")
			}
		})
	}
}
