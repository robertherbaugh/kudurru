package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var sharedValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func openStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(path, sqlite.Options{Identity: "inventory-test", InitialMode: mode, Validator: sharedValidator(), Now: func() time.Time { return testAt.Add(72 * time.Hour) }, ValidateReplicaEntry: ValidateEntry, ValidateReplicaImage: ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func newStore(t *testing.T) *sqlite.Store {
	return openStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone)
}
func initialize(t *testing.T, s *sqlite.Store) *Snapshot {
	t.Helper()
	var snap *Snapshot
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		snap, err = Initialize(ctx, tx, tx.Authority().ClusterID, testAt)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
func load(t *testing.T, s *sqlite.Store, at time.Time) *Snapshot {
	t.Helper()
	var snap *Snapshot
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		snap, err = Load(ctx, tx, tx.Authority().ClusterID, at)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
func stage(t *testing.T, s *sqlite.Store, current *Snapshot, source string, revision uint64, clients []inventory.Client, at time.Time) *Snapshot {
	t.Helper()
	next, err := inventory.PrepareReplacement(current.State, source, revision, clients, at)
	if err != nil {
		t.Fatal(err)
	}
	var snap *Snapshot
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		snap, err = Stage(ctx, tx, current.Version, next, at)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
func imageOf(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	i, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestSQLiteStageCASOriginalTimesAndForeignOwner(t *testing.T) {
	s := newStore(t)
	foreign := replication.Entry{Key: "another-owner/state", Data: json.RawMessage(`{"enabled":true}`)}
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return tx.PutSection(ctx, Section, foreign) }); err != nil {
		t.Fatal(err)
	}
	current := initialize(t, s)
	first := stage(t, s, current, "source-a", 1, []inventory.Client{client("older", "192.0.2.1", testAt)}, testAt)
	second := stage(t, s, first, "source-b", 1, []inventory.Client{client("newer", "192.0.2.1", testAt.Add(time.Minute))}, testAt.Add(time.Minute))
	removed := stage(t, s, second, "source-b", 2, nil, testAt.Add(2*time.Minute))
	got := load(t, s, testAt.Add(72*time.Hour))
	if got.Version != removed.Version || !sameState(t, got.State, removed.State) || !got.UpdatedAt.Equal(testAt.Add(2*time.Minute)) {
		t.Fatal("Load renewed evidence")
	}
	h, err := inventory.RestoreState(got.State, func() time.Time { return testAt.Add(72 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(client("x", "192.0.2.1", testAt).IPs[0], testAt.Add(3*time.Minute)) != nil {
		t.Fatal("older source resurrected")
	}
	before := imageOf(t, s)
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Stage(ctx, tx, second.Version, removed.State, testAt.Add(3*time.Minute))
		return err
	})
	if !errors.Is(err, ErrCAS) {
		t.Fatal("stale root matched", err)
	}
	after := imageOf(t, s)
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("rejected CAS changed durable generation")
	}
	var found bool
	for _, row := range after.Payload.Runtime {
		if row.Key == foreign.Key {
			found = bytes.Equal(row.Data, foreign.Data)
		}
	}
	if !found {
		t.Fatal("foreign runtime owner changed")
	}
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Initialize(ctx, tx, tx.Authority().ClusterID, testAt)
		return err
	}); !errors.Is(err, ErrInitialized) {
		t.Fatal("owner reinitialized", err)
	}
}

func TestSQLiteAtomicRollbackFinalAdmissionCancellationAndEndedTx(t *testing.T) {
	s := newStore(t)
	current := initialize(t, s)
	next, err := inventory.PrepareReplacement(current.State, "source-a", 1, []inventory.Client{client("phone", "192.0.2.1", testAt)}, testAt)
	if err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("original admission no longer current")
	before := imageOf(t, s)
	for _, mode := range []string{"callback", "final", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if _, err := Stage(ctx, tx, current.Version, next, testAt); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, Section, replication.Entry{Key: "source/head", Data: json.RawMessage(`{"revision":"1"}`)}); err != nil {
					return err
				}
				switch mode {
				case "callback":
					return rejected
				case "final":
					return tx.RequireCommitCheck(func(context.Context) error { return rejected })
				case "cancel":
					cancel()
				}
				return nil
			})
			if err == nil {
				t.Fatal("failed original admission committed")
			}
			if !before.Generation().Same(imageOf(t, s).Generation()) {
				t.Fatal("partial source/inventory commit")
			}
		})
	}
	var retained *sqlite.CommandTx
	if err := s.Command(t.Context(), s.Authority().Term, func(_ context.Context, tx *sqlite.CommandTx) error { retained = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(t.Context(), retained, current.Version, next, testAt); !errors.Is(err, sqlite.ErrTransactionEnded) {
		t.Fatal("ended tx accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Load(ctx, retained, s.Authority().Term.ClusterID, testAt); !errors.Is(err, sqlite.ErrTransactionEnded) {
		t.Fatal(err)
	}
	if _, err := Load(t.Context(), nil, "cluster-a", testAt); !errors.Is(err, sqlite.ErrTransactionEnded) {
		t.Fatal(err)
	}
	if _, err := Initialize(t.Context(), nil, "cluster-a", testAt); !errors.Is(err, sqlite.ErrTransactionEnded) {
		t.Fatal(err)
	}
	if _, err := Stage(t.Context(), nil, current.Version, next, testAt); !errors.Is(err, sqlite.ErrTransactionEnded) {
		t.Fatal(err)
	}
	// Exact original context is checked even before pure decode in a live tx.
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		dead, cancel := context.WithCancel(ctx)
		cancel()
		_, err := Load(dead, tx, tx.Authority().ClusterID, testAt)
		return err
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled request admitted", err)
	}
}

func TestSQLiteRootDigestCASAndStageCluster(t *testing.T) {
	s := newStore(t)
	current := initialize(t, s)
	next, err := inventory.PrepareReplacement(current.State, "source-a", 1, nil, testAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"generation", "digest", "wrong-cluster", "time-rollback"} {
		t.Run(mode, func(t *testing.T) {
			before := imageOf(t, s)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				expected := current.Version
				candidate := next
				at := testAt
				switch mode {
				case "generation":
					candidate.Generation++
				case "digest":
					expected.Digest = strings.Repeat("0", 64)
				case "wrong-cluster":
					_, err := Load(ctx, tx, "attacker-cluster", at)
					return err
				case "time-rollback":
					at = at.Add(-time.Nanosecond)
				}
				_, err := Stage(ctx, tx, expected, candidate, at)
				return err
			})
			if err == nil || !before.Generation().Same(imageOf(t, s).Generation()) {
				t.Fatal("stale/foreign/time-rollback admission committed", err)
			}
		})
	}
	// Root content changed with identical generation; original CAS still fails.
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		rows, err := tx.ListSectionPrefix(ctx, Section, Prefix)
		if err != nil {
			return err
		}
		root := rootOf(t, rows)
		root.UpdatedAt = testAt.Add(time.Nanosecond)
		row, err := encodeRow(RootKey, root)
		if err != nil {
			return err
		}
		if err := tx.PutSection(ctx, Section, row); err != nil {
			return err
		}
		_, err = Stage(ctx, tx, current.Version, next, testAt.Add(time.Minute))
		return err
	})
	if !errors.Is(err, ErrCAS) {
		t.Fatal("same-generation replacement root matched", err)
	}
	good := stage(t, s, current, "source-a", 1, nil, testAt)
	if good.Version.Generation != 1 {
		t.Fatal("derived cluster stage did not advance")
	}
}

func TestSQLiteConcurrentCASOneWinner(t *testing.T) {
	s := newStore(t)
	current := initialize(t, s)
	next, err := inventory.PrepareReplacement(current.State, "source-a", 1, nil, testAt)
	if err != nil {
		t.Fatal(err)
	}
	var won, lost atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := Stage(ctx, tx, current.Version, next, testAt)
				return err
			})
			if err == nil {
				won.Add(1)
			} else if errors.Is(err, ErrCAS) {
				lost.Add(1)
			} else {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 || lost.Load() != 1 || load(t, s, testAt).Version.Generation != 1 {
		t.Fatal("concurrent source commands escaped global root CAS")
	}
}

func TestSQLiteInitializeRefusesOrphansAndUnknownOwner(t *testing.T) {
	for _, key := range []string{"inventory/v9/root", ChunkPrefix + strings.Repeat("0", 64)} {
		t.Run(key, func(t *testing.T) {
			// Simulate legacy opaque rows written before owner registration.
			path := filepath.Join(t.TempDir(), sqlite.FileName)
			s, err := sqlite.Open(path, sqlite.Options{Validator: sharedValidator()})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return tx.PutSection(ctx, Section, replication.Entry{Key: key, Data: json.RawMessage(`{}`)})
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openStore(t, path, replication.Standalone)
			before := imageOf(t, s)
			err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := Initialize(ctx, tx, tx.Authority().ClusterID, testAt)
				return err
			})
			if !errors.Is(err, ErrInitialized) || !before.Generation().Same(imageOf(t, s).Generation()) {
				t.Fatal("historical owner silently initialized", err)
			}
		})
	}
}
