package servingbuild

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

func TestManagedEdgeCacheBothLeasesUnavailableAcrossLocalDenial(t *testing.T) {
	f := newManagedBuilderFixture(t, true)
	pub, err := serving.New(serving.Options{Source: f.store, Required: serving.CompleteManifest(), PreparationTimeout: 20 * time.Second, Prepare: f.builder.Prepare})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = pub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; pub.Close() })
	select {
	case <-pub.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatal("managed publisher did not initialize", pub.Status())
	}
	edge, err := NewEdgeCache(pub, "managed-node", nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := edge.State(managedBuilderContext(t))
	if err != nil || state.Available || state.AuthoritativeAvailable || state.UnavailableReason != "managed-anchors" || state.AuthoritativeReason != "managed-anchors" {
		t.Fatal("managed input granted either remote cache lease", err, state)
	}
	original := managedBuilderContext(t)
	before, err := f.store.ReadGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(before.Payload.Runtime, before.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	denial, err := f.store.CaptureAnchorDenial(original, owner.Version(), f.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(managedBuilderMessage(t, f.key, f.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := f.store.CommitAnchorDenial(verified)
	if err != nil || result.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("local denial did not commit", err)
	}
	after, err := f.store.ReadGeneration(original)
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("local denial changed canonical generation", err)
	}
	if _, err := edge.State(original); err == nil {
		t.Fatal("old publisher granted a projection after local denial")
	}
}
