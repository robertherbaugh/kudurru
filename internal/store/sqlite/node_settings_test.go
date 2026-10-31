package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func nodeLogValue(level string) NodeLogSettings {
	return NodeLogSettings{Version: 1, Node: "test-node", Level: &level}
}
func putNodeLog(t *testing.T, s *Store, n NodeLogSettings) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error { return tx.PutNodeLogSettings(ctx, n) }); err != nil {
		t.Fatal(err)
	}
}
func readLocalLog(t *testing.T, s *Store) NodeLogSettings {
	t.Helper()
	n, err := ReadNodeLogSettings(t.Context(), s.Path(), "test-node")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNodeLogLocalCommitResetAndReplicaIsolation(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t, internalOptions)
	before := imageOf(t, s)
	putNodeLog(t, s, nodeLogValue("debug"))
	if !imageOf(t, s).Generation().Same(before.Generation()) {
		t.Fatal("local commit changed canonical image")
	}
	n := readLocalLog(t, s)
	if n.Level == nil || *n.Level != "debug" {
		t.Fatal("readonly reader missed WAL commit")
	}
	var snapshot bytes.Buffer
	if err := s.Snapshot(ctx, &snapshot, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.String(), nodeLogMeta) || strings.Contains(snapshot.String(), "debug") {
		t.Fatal("local preferences escaped object snapshot")
	}
	var retained *CommandTx
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		retained = tx
		if err := tx.PutNodeLogSettings(ctx, nodeLogValue("warn")); err != nil {
			return err
		}
		staged, err := tx.NodeLogSettings(ctx, "test-node")
		if err != nil {
			return err
		}
		if staged.Level == nil || *staged.Level != "warn" {
			t.Fatal("transaction read missed staged value")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := retained.PutNodeLogSettings(ctx, n); !errors.Is(err, ErrTransactionEnded) {
		t.Fatal("retained local tx accepted", err)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path, internalOptions)
	if n = readLocalLog(t, s); n.Level == nil || *n.Level != "warn" {
		t.Fatal("local commit lost on reopen")
	}
	other, err := ReadNodeLogSettings(ctx, s.Path(), "different-node")
	if err != nil || other.Level != nil {
		t.Fatal("copied database adopted another node's overlay", err)
	}
	putNodeLog(t, s, NodeLogSettings{Version: 1, Node: "different-node"})
	if n = readLocalLog(t, s); n.Level == nil {
		t.Fatal("different node reset erased dormant overlay")
	}
	putNodeLog(t, s, NodeLogSettings{Version: 1, Node: "test-node"})
	if n = readLocalLog(t, s); n.Level != nil || n.Format != nil {
		t.Fatal("reset did not delete local overlay")
	}
	putNodeLog(t, s, nodeLogValue("warn"))
	// A real follower promotes and commits its own local settings. The former
	// primary observes that term and installs its image without adopting those
	// preferences or losing its own original local settings.
	successor := mirror(t, s)
	g := install(t, successor, imageOf(t, s))
	if _, err := successor.Promote(ctx, replication.PromoteRequest{ExpectedTerm: g.Term, ExpectedInstalled: g, RequestID: "node-log-promotion"}); err != nil {
		t.Fatal(err)
	}
	putNodeLog(t, successor, nodeLogValue("error"))
	if _, err := s.ObserveAuthority(ctx, successor.Authority().Term); err != nil {
		t.Fatal(err)
	}
	install(t, s, imageOf(t, successor))
	if n = readLocalLog(t, s); n.Level == nil || *n.Level != "warn" {
		t.Fatal("replica install replaced receiver local owner")
	}
	blank := mirror(t, successor)
	install(t, blank, imageOf(t, successor))
	if n = readLocalLog(t, blank); n.Level != nil || n.Format != nil {
		t.Fatal("replica imported sender node preferences")
	}

}

func TestNodeLogRollbackFinalChecksPoisonAndMixedCommand(t *testing.T) {
	for _, mode := range []string{"callback", "final", "poison", "cancel", "sql", "reentry", "noop-final", "local-final", "writer-final"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			putNodeLog(t, s, nodeLogValue("info"))
			before := imageOf(t, s)
			if mode == "sql" {
				if _, err := s.w.ExecContext(ctx, `CREATE TRIGGER reject_local BEFORE UPDATE ON meta WHEN NEW.key='local_node_log_v1' BEGIN SELECT RAISE(ABORT,'local failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			checked := false
			err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if err := tx.RequireCommitCheck(func(context.Context) error { checked = true; return errors.New("final refusal") }); err != nil {
					return err
				}
				value := "debug"
				if mode == "noop-final" {
					value = "info"
				}
				stageErr := tx.PutNodeLogSettings(ctx, nodeLogValue(value))
				if mode == "sql" {
					return nil
				} // ignored SQL error must poison admission
				if stageErr != nil {
					return stageErr
				}
				if mode != "noop-final" && mode != "local-final" && mode != "writer-final" {
					if _, err := tx.Apply(ctx, zone("rolled-back", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
				}
				switch mode {
				case "callback":
					return errors.New("callback refusal")
				case "poison":
					_ = tx.PutNodeLogSettings(ctx, NodeLogSettings{Version: 2, Node: "test-node"})
				case "cancel":
					cancel()
				case "writer-final":
					s.writeBlocked.Store(true)
				case "reentry":
					_, _ = s.Apply(ctx, zone("nested", "nested.example."), store.ApplyOptions{Manager: store.ManagerAPI})
				}
				return nil
			})
			s.writeBlocked.Store(false)
			if err == nil {
				t.Fatal("refused command succeeded")
			}
			if (mode == "final" || mode == "noop-final" || mode == "local-final") && !checked {
				t.Fatal("local-only/mixed no-op skipped final admission")
			}
			n := readLocalLog(t, s)
			if n.Level == nil || *n.Level != "info" || !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("refused transaction leaked local/canonical changes")
			}
		})
	}
	// A successful mixed command commits local and canonical state together.
	s := openTemp(t)
	before := imageOf(t, s)
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if err := tx.PutNodeLogSettings(ctx, nodeLogValue("debug")); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, zone("committed", "committed.example."), store.ApplyOptions{Manager: store.ManagerAPI})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if imageOf(t, s).Revision != before.Revision+1 || *readLocalLog(t, s).Level != "debug" {
		t.Fatal("mixed transaction lost atomic writes")
	}
}

func TestNodeLogStartupReadOnlyBoundedAndStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing ?#.db")
	if _, err := ReadNodeLogSettings(t.Context(), path, "test-node"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup created missing database", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := ReadNodeLogSettings(t.Context(), path, "test-node"); err != nil || n.Level != nil {
		t.Fatal("uninitialized database read failed", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("startup reader initialized the database", err)
	}
	s := openAt(t, path)
	putNodeLog(t, s, nodeLogValue("debug"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if *readLocalLog(t, s).Level != "debug" {
		t.Fatal("readonly startup failed")
	}
	if err := os.Chmod(path, info.Mode()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadNodeLogSettings(ctx, path, "test-node"); !errors.Is(err, context.Canceled) {
		t.Fatal("startup ignored cancellation", err)
	}
	for _, raw := range []string{
		strings.Repeat("x", maxNodeLogBytes+1), `{`,
		`{"version":2,"node":"test-node"}`, `{"version":1,"node":"test-node","unknown":true}`,
		`{"version":1,"node":"test-node","format":"xml"}`, `{"version":1,"node":"test-node","level":"loud"}`,
		`{"version":1,"node":"test-node"} {}`,
	} {
		if _, err := s.w.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key=?`, raw, nodeLogMeta); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadNodeLogSettings(t.Context(), path, "test-node"); err == nil {
			t.Fatal("invalid local document accepted")
		}
	}
}

func TestNodeLogFollowerCannotStage(t *testing.T) {
	p := openTemp(t)
	s := mirror(t, p)
	called := false
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		called = true
		return tx.PutNodeLogSettings(ctx, nodeLogValue("debug"))
	})
	if err == nil || called || s.Authority().Mode != replication.Follower {
		t.Fatal("follower entered local mutation", err)
	}
}
