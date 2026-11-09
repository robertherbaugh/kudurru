package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Primary activation is fixture administration. Invitation acceptance, first
// image and local term pinning below use the real mounted protocol and CLI.
func joinPrimaryFixture(t *testing.T) (*servingRuntime, *internalapi.Credentials, func(string) enrollment.Invitation) {
	return joinPrimaryLifetimeFixture(t, time.Minute)
}

func joinPrimaryLifetimeFixture(t *testing.T, lifetime time.Duration) (*servingRuntime, *internalapi.Credentials, func(string) enrollment.Invitation) {
	t.Helper()
	p := newServingRuntimeFixture(t)
	p.opts.Config.Node.DataDir = t.TempDir()
	i, err := memberidentity.Create(filepath.Join(p.opts.Config.Node.DataDir, memberIdentityFile), p.opts.Store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, cut, err := p.opts.Store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := p.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.opts.Store.Command(sqlite.WithWriterToken(ctx, w), w.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, i, "primary", listener.Addr().String())
	}); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{47}, 32)
	t.Setenv("KUDURRU_ENCRYPTION_KEY", hex.EncodeToString(key))
	t.Setenv("KUDURRU_ENCRYPTION_KEY_FILE", "")
	t.Setenv("KUDURRU_ENCRYPTION_KEY_PREVIOUS", "")
	t.Setenv("KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE", "")
	credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := p.prepareMemberListener(ctx, credentials)
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: credentials, Enrolled: enrolled})
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(serverCtx, listener) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("join listener did not stop")
		}
	})
	issue := func(name string) enrollment.Invitation {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		w, err := p.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		ctx = sqlite.WithWriterToken(ctx, w)
		image, err := p.opts.Store.ReadGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		original, err := persistence.Capture(ctx, owner, ctx.Err)
		if err != nil {
			t.Fatal(err)
		}
		var inv enrollment.Invitation
		if err := p.opts.Store.Command(ctx, w.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
			var e error
			_, inv, e = persistence.Issue(ctx, tx, original, name, "127.0.0.2:8082", time.Now().Add(lifetime))
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return inv
	}
	return p, credentials, issue
}

func TestClusterJoinActualTLSCLIAndRestart(t *testing.T) {
	p, credentials, issue := joinPrimaryFixture(t)
	inv := issue("receiver")
	wire, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "invitation")
	if err := os.WriteFile(file, []byte(wire), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "receiver")
	var output bytes.Buffer
	cmd := newClusterCmd(&cliOpts{})
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"join", "--offline", "--data-dir", dir, "--invitation-file", file, "--confirm-name", "receiver", "--confirm-address", "127.0.0.2:8082"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal("actual CLI join failed", err)
	}
	var result clusterreplica.JoinResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Primary.Committed || result.Installed.Validate() != nil {
		t.Fatal("missing independent confirmed receipts", result)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	open := func() *sqlite.Store {
		t.Helper()
		s, err := sqlite.Open(filepath.Join(dir, sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: p.opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	s := open()
	if s.Authority().Mode != replication.Follower || !s.Authority().Installed.Same(result.Installed) || s.Authority().InstallSequence != 1 {
		t.Fatal("first image and authority were not atomically retained", s.Authority())
	}
	identity, err := memberidentity.Load(filepath.Join(dir, memberIdentityFile), result.Primary.MemberID)
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.CheckAccepted(owner.State(), identity.Trust()); err != nil {
		t.Fatal("installed image does not retain original ticket", err)
	}
	audit, err := p.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range audit {
		if a.Action == "cluster.join" {
			n++
			if a.Actor != "member:"+identity.Trust().MemberID() {
				t.Fatal("wrong audit attribution")
			}
		}
	}
	if n != 1 {
		t.Fatal("acceptance audit count", n)
	}
	// A separately explicit attempt cannot overwrite this successful directory.
	if _, err := runClusterJoin(t.Context(), clusterJoinOptions{directory: dir, invitation: file, name: "receiver", address: "127.0.0.2:8082", offline: true}); err == nil {
		t.Fatal("existing receiver was overwritten")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	opts := p.opts
	cfg := *opts.Config
	cfg.Node.DataDir, cfg.Node.Name = dir, "receiver"
	opts.Config, opts.Store = &cfg, s
	runtime, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	listener, coordinator, forward, err := runtime.prepareMemberServices(t.Context(), credentials)
	if err != nil || listener == nil || coordinator == nil {
		t.Fatal("joined receiver failed retained startup", err)
	}
	runtime.member, runtime.memberForward = coordinator, forward
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := coordinator.Pull(ctx); err != nil {
		t.Fatal("joined receiver could not acquire from actual primary", err)
	}
	if s.Authority().InstallSequence != 1 {
		t.Fatal("304 was treated as a new image")
	}
	retained, err := memberidentity.Load(filepath.Join(dir, memberIdentityFile), result.Primary.MemberID)
	if err != nil || retained.Trust().Fingerprint() != identity.Trust().Fingerprint() {
		t.Fatal("restart changed identity", err)
	}
}

func TestClusterJoinWrongSharedKeyPreservesUnenrolledReceiver(t *testing.T) {
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
	t.Setenv("KUDURRU_ENCRYPTION_KEY", hex.EncodeToString(bytes.Repeat([]byte{48}, 32)))
	dir := filepath.Join(t.TempDir(), "refused")
	result, err := runClusterJoin(t.Context(), clusterJoinOptions{directory: dir, invitation: file, name: "refused", address: "127.0.0.2:8082", offline: true})
	if !errors.Is(err, clusterreplica.ErrUnauthorized) || result == nil || result.Primary.Committed {
		t.Fatal("wrong key was accepted or reported committed", err)
	}
	s, err := sqlite.Open(filepath.Join(dir, sqlite.FileName), sqlite.Options{Validator: p.opts.Validator, InitialMode: replication.Unenrolled})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Authority().Mode != replication.Unenrolled || s.Authority().Installed != (replication.Generation{}) {
		t.Fatal("failed primary acceptance changed local term")
	}
	if _, err := memberidentity.Load(filepath.Join(dir, memberIdentityFile), s.Authority().MemberID); err != nil {
		t.Fatal("failure discarded original local identity", err)
	}
	_, owner, err := p.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil || len(owner.State().Members()) != 1 {
		t.Fatal("wrong key consumed membership", err)
	}
}

func TestClusterJoinPreBodyCutAndOneTimeAcceptance(t *testing.T) {
	p, credentials, issue := joinPrimaryFixture(t)
	inv := issue("held")
	identity, err := memberidentity.Create(filepath.Join(t.TempDir(), "member.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := inv.Propose(identity)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := proposal.Encode()
	if err != nil {
		t.Fatal(err)
	}
	primary, err := memberidentity.Load(filepath.Join(p.opts.Config.Node.DataDir, memberIdentityFile), p.opts.Store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := clusterreplica.NewJoinServer(p.opts.Store, primary, credentials)
	if err != nil {
		t.Fatal(err)
	}
	var interpose atomic.Bool
	interpose.Store(true)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if interpose.Load() {
			r.Body = &joinBodyInterposition{ReadCloser: r.Body, before: func() {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				writer, err := p.opts.Store.WriterToken()
				if err != nil {
					t.Error(err)
					return
				}
				if err := p.opts.Store.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
					return tx.PutSection(ctx, "dedup", replication.Entry{Key: "join-source-interposition", Data: []byte(`{"changed":true}`)})
				}); err != nil {
					t.Error(err)
				}
			}}
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS, err = primary.EnrollmentServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	config, err := primary.Trust().EnrollmentClientTLS()
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: config, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request := func() int {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+clusterreplica.PathJoin, bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+credentials.Token())
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode
	}
	if status := request(); status != http.StatusUnauthorized {
		t.Fatal("held body adopted a replacement source", status)
	}
	_, owner, err := p.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil || len(owner.State().Members()) != 1 {
		t.Fatal("refused original body admitted member", err)
	}
	// This second request is explicit test administration. The production client
	// never retries: a new request proves only that the first refusal was atomic.
	interpose.Store(false)
	if status := request(); status != http.StatusOK {
		t.Fatal("fresh explicit acceptance failed", status)
	}
	if status := request(); status != http.StatusUnauthorized {
		t.Fatal("used invitation accepted again", status)
	}
	audit, err := p.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{})
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
		t.Fatal("refusal/replay created an acceptance audit", n)
	}
}

type joinBodyInterposition struct {
	io.ReadCloser
	before func()
}

func (b *joinBodyInterposition) Read(p []byte) (int, error) {
	if b.before != nil {
		fn := b.before
		b.before = nil
		fn()
	}
	return b.ReadCloser.Read(p)
}
