package sqlite

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
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

// zoneChunkNames lists the stored ZoneChunk names.
func zoneChunkNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	list, err := s.List(context.Background(), zonekinds.KindZoneChunk, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, item := range list.Items {
		out[item.GetName()] = true
	}
	return out
}

// referencedChunks returns the chunk names that the stored head of id resolves.
func referencedChunks(t *testing.T, b zonestate.Backend, id zonekinds.ZoneID) map[string]bool {
	t.Helper()
	head, _, err := b.Head(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := zonestate.References(head, func(d string) (*zonekinds.ZoneChunk, error) { return b.Chunk(context.Background(), d) }, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, d := range refs {
		out[zonekinds.ChunkName(d)] = true
	}
	return out
}

// A content commit deletes the chunks that no head references any more, in
// its own transaction (D405): a stream of UPDATEs keeps the stored chunks at
// what the heads reference instead of adding the whole zone content again
// for every commit. Chunks of other zones and chunks an earlier version left
// behind are handled by the same sweep: the first are kept, the second go.
func TestZoneStateCommitCollectsUnreferencedChunks(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, b := zoneStateRepository(t, s)
	ctx := context.Background()
	other := zonekinds.ZoneID{ClusterID: s.Authority().Term.ClusterID, ObjectName: "other", UID: "other-zone-uid", Apex: "example."}
	zonestatetest.Initialize(t, r, other)
	keep := referencedChunks(t, b, other)
	id := zoneStateID(s, "updates")
	v := zonestatetest.Initialize(t, r, id)
	// A chunk that an earlier version left behind: valid, staged, unreferenced.
	leftover, err := zonestate.Prepare(zonestatetest.Fixture(t, zonekinds.ZoneID{ClusterID: id.ClusterID, ObjectName: "gone", UID: "gone-uid", Apex: "example."}), nil, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range leftover.Chunks {
		if err := b.Stage(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{fmt.Sprintf("u%d.example. 60 IN A 192.0.2.%d", i%3, i+1)}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit(ctx, v.Expected, p, nil); err != nil {
			t.Fatal(i, err)
		}
		if v, err = r.Read(ctx, id); err != nil {
			t.Fatal(i, err)
		}
		want := referencedChunks(t, b, id)
		for name := range keep {
			want[name] = true
		}
		if got := zoneChunkNames(t, s); !maps.Equal(got, want) {
			t.Fatalf("commit %d: stored chunks %d, referenced by the heads %d", i, len(got), len(want))
		}
	}
	if _, err := r.Read(ctx, other); err != nil {
		t.Fatal("the other zone lost its content", err)
	}
}

// A head whose root manifest is missing references an unknown set of
// chunks: the sweep deletes nothing then and the commit still lands.
func TestZoneStateCommitKeepsChunksWhenAHeadIsUnreadable(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, b := zoneStateRepository(t, s)
	ctx := context.Background()
	broken := zonekinds.ZoneID{ClusterID: s.Authority().Term.ClusterID, ObjectName: "broken", UID: "broken-uid", Apex: "example."}
	zonestatetest.Initialize(t, r, broken)
	head, _, err := b.Head(ctx, broken)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: zonekinds.KindZoneChunk, Name: zonekinds.ChunkName(head.Spec.RootDigest)}, store.DeleteOptions{Manager: zoneStateManager})
	})
	if err != nil {
		t.Fatal(err)
	}
	id := zoneStateID(s, "updates")
	v := zonestatetest.Initialize(t, r, id)
	before := zoneChunkNames(t, s)
	p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"u.example. 60 IN A 192.0.2.9"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, v.Expected, p, nil); err != nil {
		t.Fatal(err)
	}
	after := zoneChunkNames(t, s)
	for name := range before {
		if !after[name] {
			t.Fatal("a chunk was deleted although a head could not be resolved")
		}
	}
	if len(after) <= len(before) {
		t.Fatal("the commit's own chunks are missing")
	}
}

// commitBeforeChunk is a backend that runs a hook once, just before the
// first chunk read: the window between Repository.Read's head read and its
// chunk reads, in which a content commit of the same zone can land.
type commitBeforeChunk struct {
	zonestate.Backend
	once sync.Once
	hook func()
}

func (b *commitBeforeChunk) Chunk(ctx context.Context, digest string) (*zonekinds.ZoneChunk, error) {
	b.once.Do(b.hook)
	return b.Backend.Chunk(ctx, digest)
}

// A content commit that lands between a read's head and its chunks deletes
// the chunks that head named (D405). The read must not fail for it, and
// above all must not answer store.ErrNotFound, which its callers take as
// "the zone has no head": it reads the new head instead.
func TestZoneStateReadAcrossACommitReadsTheNewHead(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, b := zoneStateRepository(t, s)
	ctx := context.Background()
	id := zoneStateID(s, "updates")
	v := zonestatetest.Initialize(t, r, id)
	p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"u.example. 60 IN A 192.0.2.9"}))
	if err != nil {
		t.Fatal(err)
	}
	var committed *zonestate.View
	reader := zonestate.NewRepository(&commitBeforeChunk{Backend: b, hook: func() {
		var err error
		if committed, err = r.Commit(ctx, v.Expected, p, nil); err != nil {
			t.Error(err)
		}
	}}, zonestate.Limits{})
	got, err := reader.Read(ctx, id)
	if err != nil {
		t.Fatalf("a read across a commit failed (not found %v): %v", errors.Is(err, store.ErrNotFound), err)
	}
	if committed == nil || got.Expected != committed.Expected {
		t.Fatalf("the read answered %+v, not the head the commit wrote", got.Expected)
	}
}

// Repository.Read against a steady stream of content commits never fails.
func TestZoneStateReadDuringSteadyCommits(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, _ := zoneStateRepository(t, s)
	ctx := context.Background()
	id := zoneStateID(s, "updates")
	v := zonestatetest.Initialize(t, r, id)
	var stop atomic.Bool
	var reads, failed atomic.Int64
	var first atomic.Value
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				_, err := r.Read(ctx, id)
				reads.Add(1)
				if err != nil {
					failed.Add(1)
					first.CompareAndSwap(nil, err.Error())
				}
			}
		})
	}
	for i := range 100 {
		p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{fmt.Sprintf("u%d.example. 60 IN A 192.0.2.%d", i%3, i%250+1)}))
		if err != nil {
			t.Fatal(err)
		}
		if v, err = r.Commit(ctx, v.Expected, p, nil); err != nil {
			t.Fatal(i, err)
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("%d reads during 100 commits", reads.Load())
	if failed.Load() != 0 {
		t.Fatalf("%d of %d reads failed during commits: %v", failed.Load(), reads.Load(), first.Load())
	}
}

// A head whose chunk is missing while the head stays put is damage, not a
// zone without a head: the read answers ErrInvalid, never store.ErrNotFound.
func TestZoneStateReadOfADamagedHeadIsNotNotFound(t *testing.T) {
	s := openTemp(t, zoneStateOptions)
	r, b := zoneStateRepository(t, s)
	ctx := context.Background()
	id := zoneStateID(s, "broken")
	zonestatetest.Initialize(t, r, id)
	head, _, err := b.Head(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: zonekinds.KindZoneChunk, Name: zonekinds.ChunkName(head.Spec.RootDigest)}, store.DeleteOptions{Manager: zoneStateManager})
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Read(ctx, id)
	if v != nil || !errors.Is(err, zonestate.ErrInvalid) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read of a damaged head: view %v, %v", v != nil, err)
	}
}
