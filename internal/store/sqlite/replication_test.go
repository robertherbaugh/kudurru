package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	internals "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func imageOf(t *testing.T, s *Store) *replication.Image {
	t.Helper()
	i, err := s.ReadGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return i
}
func mirror(t *testing.T, primary *Store) *Store {
	t.Helper()
	s := openTemp(t, internalOptions, func(o *Options) { o.InitialMode = replication.Follower })
	if err := s.Enroll(context.Background(), primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	return s
}
func install(t *testing.T, s *Store, image *replication.Image) replication.Generation {
	t.Helper()
	ctx := context.Background()
	v, err := s.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Authority()
	expected := a.Installed
	if expected.Term.ClusterID != "" {
		expected = imageOf(t, s).Generation()
	}
	g, err := s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: expected})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestReplicaCompleteReplacementPreservesPrivateStatusAndMetadata(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	z := mustApply(t, p, zone("home", "home.example."), store.ManagerFile("objects.yaml")).(*v1alpha1.DNSZone)
	z.Status.Serial = 2026101401
	if err := p.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	u := &internals.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice"}, Spec: internals.LocalUserSpec{PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA", TOTPSecret: "sealed-owner-ciphertext"}} // #nosec G101 -- Synthetic hash/ciphertext fixtures verify private image preservation; no real credentials.
	mustApply(t, p, u, store.ManagerSystem)
	_, err := p.ApplyCluster(ctx, nil, store.ApplyOptions{}, []SectionChange{{Section: "runtime", Key: "blocking", Data: json.RawMessage(`{"mode":"disabled"}`)}, {Section: "security", Key: "revocation", Data: json.RawMessage(`{"sealed":"opaque"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	s := mirror(t, p)
	first := imageOf(t, p)
	install(t, s, first)
	got := imageOf(t, s)
	left, _ := json.Marshal(got.Payload)
	right, _ := json.Marshal(first.Payload)
	var leftJSON, rightJSON any
	_ = json.Unmarshal(left, &leftJSON)
	_ = json.Unmarshal(right, &rightJSON)
	if !got.Generation().Same(first.Generation()) || !reflect.DeepEqual(leftJSON, rightJSON) {
		t.Fatal("source image changed in mirror")
	}
	if err := p.Delete(ctx, store.Key{Kind: "LocalUser", Name: "alice"}, store.DeleteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	_, err = p.ApplyCluster(ctx, nil, store.ApplyOptions{}, []SectionChange{{Section: "security", Key: "revocation", Delete: true}})
	if err != nil {
		t.Fatal(err)
	}
	install(t, s, imageOf(t, p))
	var gone internals.LocalUser
	if err := s.Get(ctx, store.Key{Kind: "LocalUser", Name: "alice"}, &gone); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted user survived: %v", err)
	}
	if len(imageOf(t, s).Payload.Security) != 0 {
		t.Fatal("deleted security row survived")
	}
	var back v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &back); err != nil {
		t.Fatal(err)
	}
	if back.Status.Serial != 2026101401 || store.ManagedBy(&back) != "file:objects.yaml" || back.UID != z.UID {
		t.Fatal("source metadata/status changed")
	}
}

func TestReplicaInstallRollbackCASIdempotenceAndWatchLineage(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	s := mirror(t, p)
	mustApply(t, p, zone("home", "home.example."), store.ManagerAPI)
	first := imageOf(t, p)
	install(t, s, first)
	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	_ = next(t, w)
	bookmark := next(t, w)
	if !strings.Contains(bookmark.Object.GetResourceVersion(), ":") || bookmark.Cursor != bookmark.Object.GetResourceVersion() {
		t.Fatal("HA bookmark is not lineage-qualified")
	}
	a := s.Authority()
	beforeSeq := a.InstallSequence
	install(t, s, first)
	if s.Authority().InstallSequence != beforeSeq {
		t.Fatal("idempotent install advanced local sequence")
	}
	mustApply(t, p, zone("new", "new.example."), store.ManagerAPI)
	second := imageOf(t, p)
	v, err := s.ValidateImage(ctx, *second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: replication.Generation{}}); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("missing CAS accepted: %v", err)
	}
	if _, err := s.w.Exec(`CREATE TRIGGER reject_replica BEFORE INSERT ON objects WHEN NEW.name='new' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: first.Generation()})
	if err == nil {
		t.Fatal("injected install succeeded")
	}
	if !imageOf(t, s).Generation().Same(first.Generation()) || !reflect.DeepEqual(s.Authority(), a) {
		t.Fatal("failed install changed rows/authority")
	}
	if _, err := s.w.Exec(`DROP TRIGGER reject_replica`); err != nil {
		t.Fatal(err)
	}
	install(t, s, second)
	select {
	case <-w.(*watcher).exited:
	case <-time.After(time.Second):
		t.Fatal("old watch survived branch replacement")
	}
	if _, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: bookmark.Cursor}); !errors.Is(err, ErrWatchLineage) {
		t.Fatalf("old cursor accepted: %v", err)
	}
	if _, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: "1"}); !errors.Is(err, ErrWatchLineage) {
		t.Fatalf("unqualified HA cursor accepted: %v", err)
	}
}

func TestFencedWritesNoopsAndPersistedAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path, internalOptions)
	z := mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	newTerm := s.Authority().Term
	newTerm.Epoch++
	newTerm.PrimaryID = "successor"
	newTerm.PromotionID = "new-term"
	if _, err := s.ObserveAuthority(ctx, newTerm); err != nil {
		t.Fatal(err)
	}
	old := WithWriterToken(ctx, token)
	for _, operation := range []func() error{
		func() error {
			_, e := s.Apply(old, z, store.ApplyOptions{Manager: store.ManagerAPI, Force: true})
			return e
		},
		func() error { return s.UpdateStatus(old, z) },
		func() error { return s.Delete(old, store.KeyOf(z), store.DeleteOptions{Force: true}) },
		func() error { return s.DeleteMany(old, nil) },
		func() error { _, e := s.ApplyCluster(old, nil, store.ApplyOptions{}, nil); return e },
	} {
		if err := operation(); !errors.Is(err, replication.ErrFenced) {
			t.Fatalf("old writer accepted: %v", err)
		}
	}
	if s.Elector("controller").IsLeader() {
		t.Fatal("fenced elector leads")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	back := openAt(t, path, internalOptions, func(o *Options) { o.InitialMode = replication.Primary })
	if back.Authority().Mode != replication.Fenced || !back.Authority().Term.Same(newTerm) {
		t.Fatal("startup options erased persisted fence")
	}
}

func TestFenceDuringOptimisticPlanIncludingNoop(t *testing.T) {
	for _, noop := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutation", true: "noop"}[noop], func(t *testing.T) {
			ctx := context.Background()
			s := openTemp(t)
			z := mustApply(t, s, zone("home", "home.example."), store.ManagerAPI).(*v1alpha1.DNSZone)
			ki := s.kinds.byKind["DNSZone"]
			reached, resume := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				_, err := s.mutate(ctx, false, func(q queryer) (*mutation, error) {
					prev, err := s.load(ctx, q, ki, store.KeyOf(z))
					if err != nil {
						return nil, err
					}
					changed := z.DeepCopy()
					if !noop {
						changed.Spec.DefaultTTL++
					}
					m, err := s.planApply(ctx, ki, store.KeyOf(z), changed, prev, store.ApplyOptions{Manager: store.ManagerAPI})
					close(reached)
					<-resume
					return m, err
				})
				result <- err
			}()
			<-reached
			term := s.Authority().Term
			term.Epoch++
			term.PrimaryID = "other"
			term.PromotionID = "other-term"
			if _, err := s.ObserveAuthority(ctx, term); err != nil {
				t.Fatal(err)
			}
			close(resume)
			if err := <-result; !errors.Is(err, replication.ErrFenced) {
				t.Fatalf("planned write crossed fence: %v", err)
			}
		})
	}
}

func TestAtomicObjectsAndOwnedSectionsRollback(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	before := imageOf(t, s)
	_, err := s.ApplyCluster(ctx, []store.Object{zone("home", "home.example.")}, store.ApplyOptions{Manager: store.ManagerAPI}, []SectionChange{{Section: "settings", Key: "bad", Data: json.RawMessage(`{}`)}})
	if !errors.Is(err, replication.ErrInvalid) {
		t.Fatalf("duplicate settings authority accepted: %v", err)
	}
	if !imageOf(t, s).Generation().Same(before.Generation()) {
		t.Fatal("partial transaction committed")
	}
	out, err := s.ApplyCluster(ctx, []store.Object{zone("home", "home.example.")}, store.ApplyOptions{Manager: store.ManagerAPI}, []SectionChange{{Section: "runtime", Key: "blocking", Data: json.RawMessage(`{"mode":"disabled"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || imageOf(t, s).Revision != before.Revision+1 {
		t.Fatal("transaction did not own one source revision")
	}
}

func TestPromotionConflictDynamicElectionAndCAS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := openTemp(t, internalOptions)
	s := mirror(t, p)
	install(t, s, imageOf(t, p))
	e := s.Elector("controller")
	started, stopped := make(chan context.Context, 1), make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx, func(term context.Context) { started <- term; <-term.Done() }, func() { stopped <- struct{}{} })
	}()
	parent := imageOf(t, s).Generation()
	req := replication.PromoteRequest{ExpectedTerm: s.Authority().Term, ExpectedInstalled: parent, RequestID: "promote-once"}
	a, err := s.Promote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Writable() || a.Parent == nil || !a.Parent.Same(parent) {
		t.Fatal("promotion lost base")
	}
	again, err := s.Promote(ctx, req)
	if err != nil || !again.Term.Same(a.Term) {
		t.Fatal("promotion retry allocated a new epoch")
	}
	var term context.Context
	select {
	case term = <-started:
	case <-time.After(time.Second):
		t.Fatal("dynamic elector did not start")
	}
	mustApply(t, s, zone("new", "new.example."), store.ManagerAPI)
	other := a.Term
	other.PrimaryID = "competing"
	other.PromotionID = "competing-term"
	if _, err := s.ObserveAuthority(ctx, other); !errors.Is(err, replication.ErrPromotionConflict) {
		t.Fatalf("same-epoch promotion accepted: %v", err)
	}
	select {
	case <-term.Done():
	case <-time.After(time.Second):
		t.Fatal("leader term was not cancelled")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("leader group did not stop")
	}
	if s.Authority().Mode != replication.Conflict || e.IsLeader() {
		t.Fatal("conflicted node remains writable")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplicationRejectsWrongClusterStaleAndFutureSchemas(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	s := mirror(t, p)
	mustApply(t, p, zone("home", "home.example."), store.ManagerAPI)
	first := imageOf(t, p)
	install(t, s, first)
	mustApply(t, p, zone("new", "new.example."), store.ManagerAPI)
	install(t, s, imageOf(t, p))
	v, err := s.ValidateImage(ctx, *first)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: s.Authority().Term, ExpectedInstalled: imageOf(t, s).Generation()})
	if !errors.Is(err, replication.ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	wrong := s.Authority().Term
	wrong.ClusterID = "different"
	if _, err := s.ObserveAuthority(ctx, wrong); !errors.Is(err, replication.ErrWrongCluster) {
		t.Fatalf("wrong cluster: %v", err)
	}
	future := *imageOf(t, p)
	future.StoreSchemaVersion++
	if _, err := s.ValidateImage(ctx, future); !errors.Is(err, replication.ErrIncompatible) {
		t.Fatalf("future schema: %v", err)
	}
}

func TestReplicaRejectsIncompleteMetadataAndSameTermCounterRollback(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	mustApply(t, p, zone("home", "home.example."), store.ManagerAPI)
	s := mirror(t, p)
	image := imageOf(t, p)
	install(t, s, image)
	for _, field := range []string{"uid", "generation", "creationTimestamp"} {
		bad := *imageOf(t, p)
		var doc map[string]any
		if err := json.Unmarshal(bad.Payload.Objects[0], &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc["metadata"].(map[string]any), field)
		bad.Payload.Objects[0], _ = json.Marshal(doc)
		if err := replication.Seal(&bad); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ValidateImage(ctx, bad); !errors.Is(err, replication.ErrInvalid) {
			t.Fatalf("missing %s: %v", field, err)
		}
	}
	bad := *imageOf(t, p)
	bad.Revision++
	bad.ObjectResourceVersion = 0
	bad.Payload.Objects = nil
	if err := replication.Seal(&bad); err != nil {
		t.Fatal(err)
	}
	v, err := s.ValidateImage(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: image.Generation()}); !errors.Is(err, replication.ErrStale) {
		t.Fatalf("source rv rolled back within term: %v", err)
	}
}
