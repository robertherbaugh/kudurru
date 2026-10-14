package kube

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Use the same full scheme/private manifest as serve.go. Missing a private
// entry otherwise makes the driver try to discover a nonexistent public CRD.
func TestEnvtestRuntimeKindsPrivateCarriersAndSafeExport(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	s := newTestStore(t, Options{Scheme: scheme, InternalKinds: private})
	if !s.WaitForSync(ctx) {
		t.Fatal("runtime kind informers did not sync")
	}
	for _, kind := range private {
		if _, err := s.List(ctx, kind, store.ListOptions{}); err != nil {
			t.Fatalf("list %s: %v", kind, err)
		}
	}
	data := []byte("private zone generation")
	digest := zonekinds.Digest(data)
	chunk := &zonekinds.ZoneChunk{ObjectMeta: metav1.ObjectMeta{Name: zonekinds.ChunkName(digest)}, Spec: zonekinds.ZoneChunkSpec{FormatVersion: zonekinds.FormatVersion, Digest: digest, Data: data}}
	budget := &integration.IntegrationBudget{ObjectMeta: metav1.ObjectMeta{Name: integration.BudgetName("runtime-test", "cloudflare", "account")}, Spec: integration.IntegrationBudgetSpec{FormatVersion: integration.FormatVersion, ClusterID: "runtime-test", Provider: "cloudflare", Account: "account", Sequence: 1, WindowStart: time.Now().UTC(), WindowSeconds: 300, Capacity: 1000}}
	for _, object := range []store.Object{chunk, budget} {
		created, err := s.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		key := store.KeyOf(created)
		t.Cleanup(func() { _ = s.Delete(context.Background(), key, store.DeleteOptions{Manager: store.ManagerSystem}) })
		if created.GetUID() == "" || created.GetResourceVersion() == "" {
			t.Fatal("missing server carrier identity")
		}
		var secret corev1.Secret
		if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "kudurru-system", Name: InternalSecretName(key.Kind, key.Namespace, key.Name)}, &secret); err != nil {
			t.Fatal(err)
		}
		if secret.Type != InternalSecretType || secret.Labels[InternalKindLabel] != string(key.Kind) || secret.UID != created.GetUID() {
			t.Fatal("runtime state did not use a private Secret carrier")
		}
		if _, err := s.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("private create-only replay: %v", err)
		}
	}
	var got zonekinds.ZoneChunk
	if err := s.Get(ctx, store.Key{Kind: zonekinds.KindZoneChunk, Name: chunk.Name}, &got); err != nil || !bytes.Equal(got.Spec.Data, data) {
		t.Fatal("zone body lost", err)
	}
	// The private model's validation still runs although it has no CRD.
	invalid := chunk.DeepCopy()
	invalid.Spec.Data = []byte("does not match content digest")
	if _, err := s.Apply(ctx, invalid, store.ApplyOptions{Manager: store.ManagerSystem}); err == nil {
		t.Fatal("private validation was bypassed")
	}
	for _, safe := range []bool{false, true} {
		var out bytes.Buffer
		if err := s.Snapshot(ctx, &out, store.SnapshotOptions{Safe: safe}); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{zonekinds.KindZoneChunk, integration.KindIntegrationBudget} {
			if strings.Contains(out.String(), "kind: "+kind) == safe {
				t.Fatalf("safe=%t export private kind %s mismatch", safe, kind)
			}
		}
	}
}
