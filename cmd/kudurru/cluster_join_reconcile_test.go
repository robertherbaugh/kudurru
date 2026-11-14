package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestClusterJoinReconcileActualTLSAfterValidationFailureAndExpiry(t *testing.T) {
	p, credentials, issue := joinPrimaryLifetimeFixture(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opts := sqlite.Options{Scheme: scheme, Validator: p.opts.Validator, InternalKinds: privateStoreKinds(), Identity: "receiver", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage}
	s, err := sqlite.CreateJoinReceiver(ctx, filepath.Join(dir, sqlite.FileName), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	local, err := memberidentity.Create(filepath.Join(dir, memberIdentityFile), s.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	inv := issue("receiver")
	wire, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "invitation")
	if err := os.WriteFile(file, []byte(wire), 0600); err != nil {
		t.Fatal(err)
	}
	validation := p.schema.PrimaryImageOptions(ctx, inv.ClusterID()).Validation
	refused := errors.New("fixture refuses first acquired image")
	validation.ValidateImage = func(*replication.Image) error { return refused }
	initial, err := clusterreplica.JoinReceiver(ctx, s, local, inv, credentials, validation)
	if !errors.Is(err, refused) || !initial.Primary.Committed || initial.Installed != (replication.Generation{}) {
		t.Fatal("fixture did not reach accepted-but-uninstalled boundary", err)
	}
	if s.Authority().Mode != replication.Unenrolled {
		t.Fatal("failed validation pinned a local term")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// This is real elapsed expiry, not a rewritten invitation or test clock.
	time.Sleep(time.Until(inv.ExpiresAt()) + time.Millisecond)
	if _, err := enrollment.ParseInvitation(wire); err == nil {
		t.Fatal("original proposal expiry was extended")
	}
	var output bytes.Buffer
	cmd := newClusterCmd(&cliOpts{})
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"join-reconcile", "--offline", "--data-dir", dir, "--invitation-file", file, "--confirm-name", "receiver", "--confirm-address", "127.0.0.2:8082"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal("actual CLI reconciliation refused", err)
	}
	var result clusterreplica.JoinReconciliationResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Observed || result.MemberID != local.Trust().MemberID() || result.Installed.Validate() != nil || bytes.Contains(output.Bytes(), []byte("committed")) {
		t.Fatal("reconciliation claimed a new commit or lost actual installation")
	}
	s, err = sqlite.OpenExisting(ctx, filepath.Join(dir, sqlite.FileName), opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authority().Mode != replication.Follower || !s.Authority().Installed.Same(result.Installed) || s.Authority().InstallSequence != 1 {
		t.Fatal("reconciliation was not retained across physical reopen")
	}
	retained, err := memberidentity.Load(filepath.Join(dir, memberIdentityFile), local.Trust().MemberID())
	if err != nil || retained.Trust().Fingerprint() != local.Trust().Fingerprint() {
		t.Fatal("reconciliation replaced private identity", err)
	}
	audit, err := p.opts.Store.ReadAudit(ctx, sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range audit {
		if a.Action == "cluster.join" {
			n++
		}
	}
	if n != 1 {
		t.Fatal("reconciliation created a second membership acceptance", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runClusterJoinReconcile(t.Context(), clusterJoinOptions{directory: dir, invitation: file, name: "receiver", address: "127.0.0.2:8082", offline: true}); err == nil {
		t.Fatal("installed receiver replayed first-image recovery")
	}
}

func TestClusterJoinReconcileWithoutPrimaryAcceptanceNeverProposes(t *testing.T) {
	p, _, issue := joinPrimaryFixture(t)
	inv := issue("refused")
	wire, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "invitation")
	if err := os.WriteFile(file, []byte(wire), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "receiver")
	o := clusterJoinOptions{directory: dir, invitation: file, name: "refused", address: "127.0.0.2:8082", offline: true}
	t.Setenv("KUDURRU_ENCRYPTION_KEY", hex.EncodeToString(bytes.Repeat([]byte{48}, 32)))
	if _, err := runClusterJoin(t.Context(), o); !errors.Is(err, clusterreplica.ErrUnauthorized) {
		t.Fatal("original proposal did not reach shared-key refusal", err)
	}
	t.Setenv("KUDURRU_ENCRYPTION_KEY", hex.EncodeToString(bytes.Repeat([]byte{47}, 32)))
	result, err := runClusterJoinReconcile(t.Context(), o)
	if err == nil || result == nil || result.Observed || result.Installed != (replication.Generation{}) {
		t.Fatal("unaccepted member acquired an image or resent proposal", err)
	}
	_, owner, err := p.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil || len(owner.State().Members()) != 1 {
		t.Fatal("reconciliation created membership", err)
	}
	audit, err := p.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range audit {
		if a.Action == "cluster.join" {
			t.Fatal("reconciliation submitted an unaccepted proposal")
		}
	}
	// Missing directories stay absent; recovery never falls into initial setup.
	o.directory = filepath.Join(t.TempDir(), "absent")
	if _, err := runClusterJoinReconcile(t.Context(), o); err == nil {
		t.Fatal("missing receiver was initialized")
	}
	if _, err := os.Stat(o.directory); !os.IsNotExist(err) {
		t.Fatal("recovery created missing directory", err)
	}
}
