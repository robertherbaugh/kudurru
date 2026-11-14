package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func integrationSourceStore(t *testing.T) (*Store, *IntegrationSourceBackend) {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	s := newTestStore(t, Options{Scheme: scheme, InternalKinds: private})
	b, err := s.IntegrationSourceBackend()
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}

// sourceVersion builds one valid head and its chunks for sequence.
func sourceVersion(owner integration.Owner, sequence uint64, parts ...string) (*integration.IntegrationSourceHead, []*integration.IntegrationSourceChunk) {
	head := &integration.IntegrationSourceHead{ObjectMeta: metav1.ObjectMeta{Name: integration.SourceHeadName(owner)}, Spec: integration.IntegrationSourceHeadSpec{FormatVersion: 1, Owner: owner, Sequence: sequence, State: "active", AcceptedAt: time.Now().UTC().Truncate(time.Second)}}
	var chunks []*integration.IntegrationSourceChunk
	for i, part := range parts {
		data := []byte(part)
		digest := integration.Digest(data)
		name := integration.SourceChunkName(owner, sequence, i, digest)
		chunks = append(chunks, &integration.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: integration.IntegrationSourceChunkSpec{FormatVersion: 1, Owner: owner, Sequence: sequence, Index: i, Digest: digest, Data: data}})
		head.Spec.Chunks = append(head.Spec.Chunks, integration.SourceChunkRef{Name: name, Digest: digest, Bytes: len(data)})
	}
	root := integration.Digest(fmt.Appendf(nil, "%d/%v", sequence, parts))
	head.Spec.RootDigest, head.Spec.ConfigDigest, head.Spec.CredentialDigest = root, integration.Digest([]byte("config")), integration.Digest([]byte("credential"))
	return head, chunks
}

func stageAll(t *testing.T, b *IntegrationSourceBackend, manager string, chunks []*integration.IntegrationSourceChunk) {
	t.Helper()
	for _, c := range chunks {
		if _, err := b.Stage(t.Context(), c, manager); err != nil {
			t.Fatal(err)
		}
	}
}

func uniqueOwner() integration.Owner {
	return integration.Owner{ClusterID: "source-test", Namespace: "default", Name: "unifi", UID: fmt.Sprintf("uid-%d", time.Now().UnixNano())}
}

// TestEnvtestIntegrationSourceRootCAS proves the Kubernetes source root
// (D384): chunks are immutable and staged first, the head is installed by one
// create or exact-resourceVersion update, a lost CAS writes nothing, and
// concurrent committers from the same predecessor produce one winner.
func TestEnvtestIntegrationSourceRootCAS(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s, b := integrationSourceStore(t)
	owner := uniqueOwner()
	manager := store.ManagerIntegration("source-test")

	// A head whose chunk was never staged is refused before any write.
	head1, chunks1 := sourceVersion(owner, 1, "first-a", "first-b")
	if _, err := b.Commit(ctx, "", head1, chunks1, manager); !errors.Is(err, ErrSourceCorrupt) {
		t.Fatal("commit without durable chunks", err)
	}
	if _, err := b.Head(ctx, owner); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("refused commit wrote a head", err)
	}
	stageAll(t, b, manager, chunks1)
	// Staging the same immutable bytes again is the same chunk.
	again, err := b.Stage(ctx, chunks1[0], manager)
	if err != nil || again.UID == "" {
		t.Fatal("identical restage refused", err)
	}
	var sec corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "kudurru-system", Name: InternalSecretName(integration.KindIntegrationSourceChunk, "", chunks1[0].Name)}, &sec); err != nil || sec.Immutable == nil || !*sec.Immutable {
		t.Fatal("chunk carrier is not an immutable Secret", err)
	}
	// A first head must be sequence 1 and a create.
	if _, err := b.Commit(ctx, "123", head1, chunks1, manager); !errors.Is(err, ErrSourceConflict) || !errors.Is(err, store.ErrConflict) {
		t.Fatal("update of an absent head", err)
	}
	committed, err := b.Commit(ctx, "", head1, chunks1, manager)
	if err != nil {
		t.Fatal(err)
	}
	if committed.ResourceVersion == "" || committed.UID == "" || committed.Spec.Sequence != 1 {
		t.Fatal("committed head lacks server identity")
	}
	// A second create from "nothing" loses.
	if _, err := b.Commit(ctx, "", head1, chunks1, manager); !errors.Is(err, ErrSourceConflict) {
		t.Fatal("second create from absent", err)
	}

	// Two committers from the same predecessor: exactly one wins with n+1.
	type result struct {
		head *integration.IntegrationSourceHead
		err  error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, part := range []string{"left", "right"} {
		head, chunks := sourceVersion(owner, 2, part)
		stageAll(t, b, manager, chunks)
		wg.Go(func() {
			h, err := b.Commit(ctx, committed.ResourceVersion, head, chunks, manager)
			results <- result{h, err}
		})
	}
	wg.Wait()
	close(results)
	winners, losers := 0, 0
	var winner *integration.IntegrationSourceHead
	for r := range results {
		switch {
		case r.err == nil:
			winners++
			winner = r.head
		case errors.Is(r.err, ErrSourceConflict):
			losers++
		default:
			t.Fatal("unexpected commit error", r.err)
		}
	}
	if winners != 1 || losers != 1 || winner.Spec.Sequence != 2 {
		t.Fatal("concurrent committers", winners, losers)
	}
	current, chunks, err := b.Read(ctx, owner)
	if err != nil || current.ResourceVersion != winner.ResourceVersion || current.Spec.RootDigest != winner.Spec.RootDigest || len(chunks) != 1 {
		t.Fatal("read did not return the winning source", err)
	}

	// A lost CAS (stale predecessor) writes nothing.
	stale, staleChunks := sourceVersion(owner, 3, "stale")
	stageAll(t, b, manager, staleChunks)
	if _, err := b.Commit(ctx, committed.ResourceVersion, stale, staleChunks, manager); !errors.Is(err, ErrSourceConflict) {
		t.Fatal("stale predecessor committed", err)
	}
	after, err := b.Head(ctx, owner)
	if err != nil || after.ResourceVersion != winner.ResourceVersion {
		t.Fatal("lost CAS changed the head", err)
	}
	// A skipped sequence is invalid even from the current predecessor.
	skip, skipChunks := sourceVersion(owner, 4, "skip")
	stageAll(t, b, manager, skipChunks)
	if _, err := b.Commit(ctx, winner.ResourceVersion, skip, skipChunks, manager); !errors.Is(err, ErrSourceInvalid) {
		t.Fatal("skipped sequence", err)
	}
	// The listing sees one head.
	heads, err := b.Heads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, h := range heads {
		if h.Spec.Owner == owner {
			found++
		}
	}
	if found != 1 {
		t.Fatal("heads per owner", found)
	}
	_ = s
}

// TestEnvtestIntegrationSourceCarrierIntegrity: an immutable chunk replayed
// with other bytes refuses, an undecodable head is an error on Get and List
// (never an absent source), a missing referenced chunk is corrupt, and a
// guarded chunk delete never removes a successor UID.
func TestEnvtestIntegrationSourceCarrierIntegrity(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s, b := integrationSourceStore(t)
	owner := uniqueOwner()
	manager := store.ManagerIntegration("source-test")
	head, chunks := sourceVersion(owner, 1, "integrity")
	stageAll(t, b, manager, chunks)

	// Rewrite the chunk carrier so the same name holds a different spec. A
	// replay of the original bytes must not accept it.
	name := InternalSecretName(integration.KindIntegrationSourceChunk, "", chunks[0].Name)
	var sec corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "kudurru-system", Name: name}, &sec); err != nil {
		t.Fatal(err)
	}
	if err := shared.admin.Delete(ctx, &sec); err != nil {
		t.Fatal(err)
	}
	other := chunks[0].DeepCopy()
	other.Spec.FormatVersion = 2 // still validates name/digest, different spec
	ki, _ := s.internal.get(integration.KindIntegrationSourceChunk)
	data, err := encodeObject(ki, other)
	if err != nil {
		t.Fatal(err)
	}
	forged := s.internalSecret(ki, other, data)
	if err := shared.admin.Create(ctx, forged); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stage(ctx, chunks[0], manager); err == nil {
		t.Fatal("restage over different stored bytes accepted")
	}
	if _, err := b.Commit(ctx, "", head, chunks, manager); err == nil {
		t.Fatal("commit over different stored bytes accepted")
	}
	if err := shared.admin.Delete(ctx, forged); err != nil {
		t.Fatal(err)
	}
	stageAll(t, b, manager, chunks)
	committed, err := b.Commit(ctx, "", head, chunks, manager)
	if err != nil {
		t.Fatal(err)
	}

	// A referenced chunk that disappeared makes the source corrupt.
	stored, _, err := b.Read(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	var chunk integration.IntegrationSourceChunk
	if err := s.Get(ctx, store.Key{Kind: integration.KindIntegrationSourceChunk, Name: stored.Spec.Chunks[0].Name}, &chunk); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteChunk(ctx, &chunk, manager); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Read(ctx, owner); !errors.Is(err, ErrSourceCorrupt) {
		t.Fatal("missing chunk read as a source", err)
	}
	// The guarded delete of the old incarnation never removes a successor.
	successor, err := b.Stage(ctx, chunks[0], manager)
	if err != nil {
		t.Fatal(err)
	}
	if successor.UID == chunk.UID {
		t.Fatal("successor has the old UID")
	}
	if err := b.DeleteChunk(ctx, &chunk, manager); !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
		t.Fatal("guarded delete of the old UID", err)
	}
	if _, _, err := b.Read(ctx, owner); err != nil {
		t.Fatal("successor chunk was removed", err)
	}

	// An undecodable head carrier is an error on Get and List.
	headName := InternalSecretName(integration.KindIntegrationSourceHead, "", committed.Name)
	var headSecret corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "kudurru-system", Name: headName}, &headSecret); err != nil {
		t.Fatal(err)
	}
	headSecret.Data[InternalObjectKey] = []byte(`{"apiVersion":"kudurru.io/v1alpha1","kind":"IntegrationSourceHead","metadata":{"name":"` + committed.Name + `"},"spec":{"unknown":true}}`)
	if err := shared.admin.Update(ctx, &headSecret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.admin.Delete(context.Background(), &headSecret) })
	if _, err := b.Head(ctx, owner); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatal("undecodable head read as absent or valid", err)
	}
	if _, err := b.Heads(ctx); err == nil {
		t.Fatal("undecodable head skipped by the listing")
	}
	if _, err := s.List(ctx, integration.KindIntegrationSourceHead, store.ListOptions{}); err == nil {
		t.Fatal("undecodable head skipped by List")
	}
}
