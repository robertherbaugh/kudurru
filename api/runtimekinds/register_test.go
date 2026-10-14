package runtimekinds_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zone "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPrivateRuntimeCarrierReplicationAndSafeExport(t *testing.T) {
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, k := range runtimekinds.Private() {
		private = append(private, store.Kind(k))
	}
	open := func(mode replication.Mode) *sqlite.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: v, InitialMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	primary := open(replication.Primary)
	ctx := context.Background()
	data := []byte("private committed zone body")
	digest := zone.Digest(data)
	chunk := &zone.ZoneChunk{ObjectMeta: metav1.ObjectMeta{Name: zone.ChunkName(digest)}, Spec: zone.ZoneChunkSpec{FormatVersion: zone.FormatVersion, Digest: digest, Data: data}}
	budget := &integration.IntegrationBudget{ObjectMeta: metav1.ObjectMeta{Name: integration.BudgetName("cluster", "cloudflare", "account")}, Spec: integration.IntegrationBudgetSpec{FormatVersion: integration.FormatVersion, ClusterID: "cluster", Provider: "cloudflare", Account: "account", Sequence: 1, WindowStart: time.Now().UTC(), WindowSeconds: 300, Capacity: 1000}}
	sources := sourceCarriers(primary.Info().ClusterID)
	for _, object := range append([]store.Object{chunk, budget}, sources...) {
		if _, err := primary.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	for _, safe := range []bool{false, true} {
		var b bytes.Buffer
		if err := primary.Snapshot(ctx, &b, store.SnapshotOptions{Safe: safe}); err != nil {
			t.Fatal(err)
		}
		for _, k := range append([]string{zone.KindZoneChunk, integration.KindIntegrationBudget}, integration.SourceKinds...) {
			if strings.Contains(b.String(), "kind: "+k) == safe {
				t.Fatalf("safe=%t export private kind %s mismatch", safe, k)
			}
		}
	}
	image, err := primary.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range runtimekinds.Private() {
		if !slices.ContainsFunc(image.KindManifest, func(m replication.Kind) bool { return m.Kind == k }) {
			t.Fatalf("missing explicit private manifest %s", k)
		}
	}
	follower := open(replication.Follower)
	if err := follower.Enroll(ctx, primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: follower.Authority().Term}); err != nil {
		t.Fatal(err)
	}
	var restored zone.ZoneChunk
	if err := follower.Get(ctx, store.Key{Kind: zone.KindZoneChunk, Name: chunk.Name}, &restored); err != nil || !bytes.Equal(restored.Spec.Data, data) {
		t.Fatal("private zone data lost on replication", err)
	}
	var copied integration.IntegrationBudget
	if err := follower.Get(ctx, store.Key{Kind: integration.KindIntegrationBudget, Name: budget.Name}, &copied); err != nil || copied.Spec.Capacity != 1000 {
		t.Fatal("shared provider budget lost on replication", err)
	}
	for _, want := range sources {
		original := want.DeepCopyObject().(store.Object)
		if err := primary.Get(ctx, store.KeyOf(want), original); err != nil {
			t.Fatal(err)
		}
		got := want.DeepCopyObject().(store.Object)
		if err := follower.Get(ctx, store.KeyOf(want), got); err != nil {
			t.Fatal("source carrier lost on replication", err)
		}
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(got)
		if !bytes.Equal(a, b) {
			t.Fatal("source carrier identity or evidence changed on replication")
		}
		if err := primary.Delete(ctx, store.KeyOf(want), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	installed := image.Generation()
	image, err = primary.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range integration.SourceKinds {
		if !slices.ContainsFunc(image.KindManifest, func(m replication.Kind) bool { return m.Kind == kind && m.Count == 0 }) {
			t.Fatal("deleted source kind missing explicit empty manifest", kind)
		}
	}
	validated, err = follower.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: follower.Authority().Term, ExpectedInstalled: installed}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range integration.SourceKinds {
		rows, err := follower.List(ctx, store.Kind(kind), store.ListOptions{})
		if err != nil || len(rows.Items) != 0 {
			t.Fatal("empty replacement retained former source carrier", kind, err)
		}
	}
	for _, object := range []store.Object{&public.ServiceAccount{}, &public.RoleBinding{}, &public.IdentityProvider{}} {
		gvks, _, err := scheme.ObjectKinds(object)
		if err != nil || len(gvks) != 1 {
			t.Fatal("public IAM registered more than once", gvks, err)
		}
	}
}

func sourceCarriers(cluster string) []store.Object {
	owner := integration.Owner{ClusterID: cluster, Namespace: "default", Name: "unifi", UID: "source-uid"}
	now := time.Now().UTC().Truncate(time.Second)
	data := []byte("original bounded source evidence")
	digest := integration.Digest(data)
	chunk := &integration.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: integration.SourceChunkName(owner, 1, 0, digest)}, Spec: integration.IntegrationSourceChunkSpec{FormatVersion: 1, Owner: owner, Sequence: 1, Digest: digest, Data: data}}
	head := &integration.IntegrationSourceHead{ObjectMeta: metav1.ObjectMeta{Name: integration.SourceHeadName(owner)}, Spec: integration.IntegrationSourceHeadSpec{FormatVersion: 1, Owner: owner, Sequence: 1, ConfigDigest: digest, CredentialDigest: digest, RootDigest: digest, AcceptedAt: now, State: "active", Chunks: []integration.SourceChunkRef{{Name: chunk.Name, Digest: digest, Bytes: len(data)}}}}
	scope := integration.AllocationScope{ClusterID: cluster, ZoneUID: "zone-uid", Apex: "example."}
	allocation := &integration.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: integration.NameAllocationName(scope, "client")}, Spec: integration.IntegrationNameAllocationSpec{FormatVersion: 1, Scope: scope, Label: "client", Owner: owner, ClientID: "client-id", Base: "client", CreatedAt: now}}
	return []store.Object{chunk, head, allocation}
}
