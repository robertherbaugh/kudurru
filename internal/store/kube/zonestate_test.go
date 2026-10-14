package kube

import (
	"context"
	"errors"
	"testing"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"github.com/robertherbaugh/kudurru/internal/zonestate/zonestatetest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func zoneStateOptions(t testing.TB) Options {
	t.Helper()
	s := testScheme()
	if err := zonekinds.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range zonekinds.Kinds {
		private = append(private, store.Kind(kind))
	}
	return Options{Scheme: s, InternalKinds: private}
}
func zoneStateRepository(t testing.TB, s *Store) (*zonestate.Repository, zonestate.Backend) {
	t.Helper()
	b, err := s.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return zonestate.NewRepository(b, zonestate.Limits{}), b
}
func zoneStateID(name string) zonekinds.ZoneID {
	return zonekinds.ZoneID{ClusterID: "envtest-cluster", ObjectName: name, UID: "test-zone-uid", Apex: "example."}
}

func TestZoneStateConcurrencyAndRecovery(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, zoneStateOptions(t))
	r, b := zoneStateRepository(t, s)
	zonestatetest.Run(t, r, b, s, zoneStateID("kube"))
}

func TestZoneStateImmutableChunksAndColdDriver(t *testing.T) {
	requireEnv(t)
	opts := zoneStateOptions(t)
	s := newTestStore(t, opts)
	r, _ := zoneStateRepository(t, s)
	id := zoneStateID("cold")
	v := zonestatetest.Initialize(t, r, id)
	p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"accepted.example. 60 IN A 192.0.2.8"}))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := r.Commit(context.Background(), v.Expected, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sec corev1.Secret
	c := p.Chunks[0]
	key := client.ObjectKey{Namespace: s.opts.Namespace, Name: InternalSecretName(zonekinds.KindZoneChunk, "", c.Name)}
	if err := s.c.Get(context.Background(), key, &sec); err != nil {
		t.Fatal(err)
	}
	if sec.Immutable == nil || !*sec.Immutable {
		t.Fatal("chunk Secret is mutable")
	}
	sec.Data[InternalObjectKey] = []byte("corrupt")
	if err := s.c.Update(context.Background(), &sec); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("apiserver accepted chunk mutation: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cold := newTestStore(t, opts)
	rr, _ := zoneStateRepository(t, cold)
	fresh, err := rr.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Head.Spec.RootDigest != committed.Head.Spec.RootDigest || fresh.Head.Spec.Serial != committed.Head.Spec.Serial {
		t.Fatal("cold driver lost durable root")
	}
}

func TestZoneStateOutageRejectsAndPreservesHead(t *testing.T) {
	requireEnv(t)
	cfg, down := partitionedConfig(shared.cfg)
	s := newStoreOn(t, cfg, zoneStateOptions(t))
	r, _ := zoneStateRepository(t, s)
	id := zoneStateID("outage")
	v := zonestatetest.Initialize(t, r, id)
	p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"unavailable.example. 60 IN A 192.0.2.8"}))
	if err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	if _, err := r.Commit(context.Background(), v.Expected, p, nil); err == nil {
		t.Fatal("accepted UPDATE while apiserver unavailable")
	}
	down.Store(false)
	fresh, err := r.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Head.Spec.RootDigest != v.Head.Spec.RootDigest {
		t.Fatal("outage partially published candidate")
	}
	bad := *p
	bad.Chunks = p.Chunks[:1]
	if _, err := r.Commit(context.Background(), v.Expected, &bad, nil); err == nil || errors.Is(err, zonestate.ErrConflict) {
		t.Fatalf("incomplete candidate did not fail validation: %v", err)
	}
}
