package sqlite

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"github.com/robertherbaugh/kudurru/internal/zonestate/zonestatetest"
	"k8s.io/apimachinery/pkg/runtime"
)

func zoneStateOptions(o *Options) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := zonekinds.AddToScheme(s); err != nil {
		panic(err)
	}
	o.Scheme = s
}
func zoneStateRepository(t testing.TB, s *Store) (*zonestate.Repository, zonestate.Backend) {
	t.Helper()
	b, err := s.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return zonestate.NewRepository(b, zonestate.Limits{}), b
}
func zoneStateID(s *Store, name string) zonekinds.ZoneID {
	return zonekinds.ZoneID{ClusterID: s.Authority().Term.ClusterID, ObjectName: name, UID: "test-zone-uid", Apex: "example."}
}

func TestZoneStateConcurrencyAndRecovery(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, b := zoneStateRepository(t, s)
	zonestatetest.Run(t, r, b, s, zoneStateID(s, "sqlite"))
}

func TestZoneStateTermAndTransactionRollback(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, _ := zoneStateRepository(t, s)
	id := zoneStateID(s, "fenced")
	v := zonestatetest.Initialize(t, r, id)
	p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"new.example. 60 IN A 192.0.2.8"}))
	if err != nil {
		t.Fatal(err)
	}
	e := v.Expected
	e.Term.Epoch++
	if _, err := r.Commit(context.Background(), e, p, nil); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("stale term committed: %v", err)
	}
	fault := errors.New("crash after transaction stages head")
	err = s.Command(context.Background(), v.Expected.Term, func(ctx context.Context, tx *CommandTx) error {
		if _, err := tx.CommitZoneState(ctx, v.Expected, p, zonestate.Limits{}, nil); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	fresh, err := r.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Head.Spec.RootDigest != v.Head.Spec.RootDigest || fresh.Head.Spec.Serial != v.Head.Spec.Serial {
		t.Fatal("rollback published transaction")
	}
}

func TestZoneStateAbruptRestartDurableCommit(t *testing.T) {
	// Child exits without Close; no serving/local snapshot recovery can mask
	// whether the actual state.db commit survived process interruption.
	if path := os.Getenv("KUDURRU_ZONE_CRASH_DB"); path != "" {
		s, err := Open(path, testOptions(zoneStateOptions))
		if err != nil {
			t.Fatal(err)
		}
		r, _ := zoneStateRepository(t, s)
		id := zoneStateID(s, "restart")
		v := zonestatetest.Initialize(t, r, id)
		p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"accepted.example. 60 IN A 192.0.2.8"}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit(context.Background(), v.Expected, p, nil); err != nil {
			t.Fatal(err)
		}
		os.Exit(23)
	}
	path := filepath.Join(t.TempDir(), FileName)
	cmd := exec.Command(os.Args[0], "-test.run=^TestZoneStateAbruptRestartDurableCommit$") //nolint:gosec // G204: exact current test binary and fixed test selector.
	cmd.Env = append(os.Environ(), "KUDURRU_ZONE_CRASH_DB="+path)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child did not hit post-commit crash point: %v\n%s", err, out)
	}
	s := openAt(t, path, zoneStateOptions)
	r, _ := zoneStateRepository(t, s)
	v, err := r.Read(context.Background(), zoneStateID(s, "restart"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Head.Spec.CommitSequence != 2 || v.Head.Spec.Serial != 1_800_000_000 {
		t.Fatal("restart lost accepted serial/generation")
	}
	rrs, err := v.Aggregate.RRs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rr := range rrs {
		if rr.Header().Name == "accepted.example." {
			found = true
		}
	}
	if !found {
		t.Fatal("accepted update missing after abrupt restart")
	}
}
