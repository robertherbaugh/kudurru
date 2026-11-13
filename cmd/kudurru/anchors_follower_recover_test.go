package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// F28-HA-4 (D369), one step in process: offline recovery needs the
// database's own writer term. A follower that installed its
// primary's quarantined image is refused before any acquisition, and its
// database (owner, carrier, authority and audit) is unchanged. The full
// enrolled flow runs in the e2e TestEnrolledManagedAnchorsOfflineRecovery.
func TestOfflineAnchorRecoverRefusedOnFollowerDatabase(t *testing.T) {
	primaryPath, lost := offlineAnchorFixture(t)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	options := sqlite.Options{Scheme: scheme, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage}
	primary, err := sqlite.Open(primaryPath, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	followerOptions := options
	followerOptions.InitialMode, followerOptions.Identity = replication.Follower, "quarantined-follower"
	followerPath := filepath.Join(t.TempDir(), sqlite.FileName)
	follower, err := sqlite.Open(followerPath, followerOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = follower.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := follower.Enroll(ctx, primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx := captureServingReplica(t, follower)
	expected := follower.Authority().Installed
	image, err := primary.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := follower.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: expected}); err != nil || !installed.Same(image.Generation()) {
		t.Fatal("follower did not install the quarantined image", err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	read := anchorOfflineOptions{database: followerPath, offline: true, timeout: 10 * time.Second}
	before, err := runAnchorOffline(t.Context(), read, false)
	if err != nil || !before.Quarantined || before.Version != lost.Version() || before.Authority == nil || before.Authority.Mode != string(replication.Follower) {
		t.Fatalf("follower status before recovery: %+v %v", before, err)
	}
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	recovery := read
	recovery.expected, recovery.reason, recovery.ds, recovery.servers = lost.Version().Digest, "recover on a follower database", seed.DS, []string{endpoint.String()}
	result, err := runAnchorOffline(t.Context(), recovery, true)
	if !errors.Is(err, replication.ErrReadOnly) || result == nil || result.Outcome != "not-attempted" {
		t.Fatalf("recover on a follower database: %+v %v, want not-attempted with %v", result, err, replication.ErrReadOnly)
	}
	if reads.Load() != 0 {
		t.Fatal("recover on a follower database acquired", reads.Load())
	}
	after, err := runAnchorOffline(t.Context(), read, false)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused recovery changed the follower database:\n%+v\n%+v %v", before, after, err)
	}
	auditCtx, stopAudit := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopAudit()
	s, err := sqlite.OpenExisting(auditCtx, followerPath, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	entries, err := s.ReadAudit(auditCtx, sqlite.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Action == "trustanchors.recover" {
			t.Fatal("refused follower recovery committed an audit row")
		}
	}
}
