package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Fixture administration installs the first complete enrolled image explicitly.
// Subsequent generations use the actual root listener and acquisition worker.
// This does not stand in for the separate initial remote-join protocol.
func TestRootOperatorPromotionActualCLICommitAndRestart(t *testing.T) {
	primary := newServingRuntimeFixture(t)
	primary.opts.Config.Node.DataDir = t.TempDir()
	token, tokenID := tokens.Generate()
	viewer, viewerID := tokens.Generate()
	revoked, revokedID := tokens.Generate()
	err := primary.opts.Store.Command(t.Context(), primary.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "operator"}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		for _, proof := range []struct{ id, wire, scope string }{{tokenID, token, "admin"}, {viewerID, viewer, "viewer"}, {revokedID, revoked, "admin"}} {
			_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: proof.id}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(proof.wire), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:operator", Provider: "local"}, Scopes: []string{proof.scope}}}, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	open := func() *sqlite.Store {
		s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator,
			InternalKinds: privateStoreKinds(), InitialMode: replication.Follower,
			ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	mirror := open()
	local, err := memberidentity.Create(filepath.Join(primary.opts.Config.Node.DataDir, memberIdentityFile), primary.opts.Store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := memberidentity.Create(filepath.Join(dir, memberIdentityFile), mirror.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	finite := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	write := func(fn func(context.Context, *sqlite.CommandTx) error) {
		writer, err := primary.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := primary.opts.Store.Command(sqlite.WithWriterToken(finite(), writer), writer.Term(), fn); err != nil {
			t.Fatal(err)
		}
	}
	_, cut, err := primary.opts.Store.ReadServingCandidate(finite())
	if err != nil {
		t.Fatal(err)
	}
	write(func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, local, "primary", listener.Addr().String())
	})
	change := func(fn func(context.Context, *sqlite.CommandTx, *persistence.Original) error) {
		ctx := finite()
		writer, err := primary.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		ctx = sqlite.WithWriterToken(ctx, writer)
		image, err := primary.opts.Store.ReadGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		original, err := persistence.Capture(ctx, owner, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := primary.opts.Store.Command(ctx, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error { return fn(ctx, tx, original) }); err != nil {
			t.Fatal(err)
		}
	}
	var invitation enrollment.Invitation
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		var err error
		_, invitation, err = persistence.Issue(ctx, tx, original, "receiver", "127.0.0.2:8082", time.Now().Add(time.Minute))
		return err
	})
	proposal, err := invitation.Propose(remote)
	if err != nil {
		t.Fatal(err)
	}
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		_, err := persistence.Accept(ctx, tx, original, proposal)
		return err
	})
	if err := mirror.Enroll(finite(), primary.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	install := captureServingReplica(t, mirror)
	image, err := primary.opts.Store.ReadGeneration(install)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := mirror.ValidateImage(install, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.InstallReplica(install, validated, replication.InstallOptions{ExpectedTerm: image.Term}); err != nil {
		t.Fatal(err)
	}
	credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{49}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := primary.prepareMemberListener(t.Context(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: credentials, Enrolled: enrolled})
	if err != nil {
		t.Fatal(err)
	}
	serverContext, stopServer := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverContext, listener) }()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("actual member listener did not join")
		}
	})
	start := func() (*servingRuntime, func()) {
		opts := primary.opts
		config := *opts.Config
		config.Node.DataDir, config.Node.Name = dir, "receiver"
		opts.Config, opts.Store = &config, mirror
		s, err := newServingRuntime(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		setup, stopSetup := context.WithTimeout(t.Context(), 5*time.Second)
		listener, coordinator, err := s.prepareMemberServices(setup, credentials)
		s.member = coordinator
		stopSetup()
		if err != nil || listener == nil || s.member == nil {
			t.Fatal("retained follower did not mount acquisition", err)
		}
		// Setup is already canceled: the network exchange must own its own
		// original context and pure local schema validation lifetime.
		if err := s.member.Pull(finite()); err != nil {
			t.Fatal("setup lifetime leaked into actual acquisition", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("retained acquisition runtime did not join")
			}
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(stop)
		return s, stop
	}
	s, stop := start()
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	h, err := newServingHTTP(servingHTTPOptions{Runtime: s, Hasher: users.NewHasher(1)})
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Options{Store: mirror, Validator: s.opts.Validator, Config: s.opts.Config, CanonicalSettings: s, Authenticate: h.Middleware, AuthHandler: h.Handler(), Authorizer: h.Authorizer(), Roles: h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); router.ServeHTTP(w, r) }))
	defer srv.Close()
	execute := func(proof string, args ...string) (string, error) {
		var out strings.Builder
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", srv.URL, "--token", proof, "-o", "json", "cluster"}, args...))
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	output, err := execute(token, "promotion")
	var review api.ClusterPromotionInfo
	if err != nil || json.Unmarshal([]byte(output), &review) != nil || review.Eligible || !review.Installed.Same(image.Generation()) {
		t.Fatal("missing exact reachable follower review", err, output)
	}
	reviewPath := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(reviewPath, []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"promote", "--review", reviewPath, "--confirm-node", "receiver", "--request-id", "reviewed-loss", "--reason", "operator reviewed primary outage"}
	if _, err := execute(token, args...); err == nil {
		t.Fatal("reachable primary promoted")
	}
	if _, err := s.member.Promote(finite(), clusterreplica.PromoteRequest{ConfirmNode: "receiver"}); !errors.Is(err, clusterreplica.ErrForbidden) {
		t.Fatal("background worker gained authority", err)
	}
	// Stop the actual TLS server. Eligibility uses real scheduled failed pulls
	// and the production monotonic two-interval window, not a forged timestamp.
	stopServer()
	deadline := time.Now().Add(22 * time.Second)
	for !s.member.Status(finite()).Eligible {
		if time.Now().After(deadline) {
			t.Fatal("actual primary loss never became eligible", s.member.Status(finite()))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Slow actual HTTP body: later framework deadline changes cannot renew
	// the earlier original request. No promotion/audit effect may occur.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 150*time.Millisecond)
		defer cancel()
		router.ServeHTTP(w, r.WithContext(ctx))
	}))
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(slow.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	beforeSlow := mirror.Authority()
	began := time.Now()
	_, err = conn.Write([]byte("POST /api/v1/cluster/promote HTTP/1.1\r\nHost: kudurru.test\r\nAuthorization: Bearer " + token + "\r\nContent-Type: application/json\r\nContent-Length: 64000\r\n\r\n{"))
	if err != nil {
		t.Fatal(err)
	}
	response, readErr := http.ReadResponse(bufio.NewReader(conn), nil)
	if response != nil {
		_ = response.Body.Close()
		if response.StatusCode < 400 {
			t.Fatal("incomplete promotion body accepted")
		}
	}
	if time.Since(began) > time.Second {
		t.Fatal("slow body extended original lifetime", readErr)
	}
	_ = conn.Close()
	slow.Close()
	if !mirror.Authority().Term.Same(beforeSlow.Term) {
		t.Fatal("slow body changed authority")
	}
	n := requests.Load()
	if _, err := execute(viewer, args...); err == nil || requests.Load() != n+1 {
		t.Fatal("viewer promoted or request replayed", err)
	}
	bad := append([]string(nil), args...)
	bad[4] = "other"
	n = requests.Load()
	if _, err := execute(token, bad...); err == nil || requests.Load() != n {
		t.Fatal("wrong local node performed network mutation", err)
	}

	// Capture a real original HTTP proof, then independently install a source
	// revoking it during body acquisition. The old operation must not recapture.
	reviewed := api.ClusterPromoteCommand{ClusterPromotionReview: review.ClusterPromotionReview, ConfirmNode: "receiver", RequestID: "revoked-original", Reason: "held original proof"}
	interposed := false
	body := &clusterActivationBody{Reader: strings.NewReader(roleBindingBody(t, reviewed)), before: func() {
		expected := mirror.Authority().Installed
		install := captureServingReplica(t, mirror)
		write(func(ctx context.Context, tx *sqlite.CommandTx) error {
			return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: revokedID}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true})
		})
		successor, err := primary.opts.Store.ReadGeneration(install)
		if err != nil {
			t.Fatal(err)
		}
		validated, err := mirror.ValidateImage(install, *successor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mirror.InstallReplica(install, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: expected}); err != nil {
			t.Fatal(err)
		}
		interposed = true
	}}
	held := httptest.NewRequest("POST", "/api/v1/cluster/promote", body)
	held.Header.Set("Authorization", "Bearer "+revoked)
	held.Header.Set("Content-Type", "application/json")
	heldResult := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder()}
	router.ServeHTTP(heldResult, held)
	if !interposed || heldResult.Code < 400 || mirror.Authority().Mode != replication.Follower {
		t.Fatal("held revoked proof refreshed or promoted", heldResult.Code)
	}
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	// A distinct explicit operator review follows the refused original request.
	// This is a new decision, never automatic request replay or source rescue.
	output, err = execute(token, "promotion")
	if err != nil || json.Unmarshal([]byte(output), &review) != nil {
		t.Fatal("new operator review unavailable", err)
	}
	stale := review
	stale.Installed.Digest = strings.Repeat("a", 64)
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	n = requests.Load()
	if _, err := execute(token, args...); err == nil || requests.Load() != n+1 {
		t.Fatal("stale reviewed source refreshed or promoted", err)
	}
	if err := os.WriteFile(reviewPath, []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	n = requests.Load()
	output, err = execute(token, args...)
	if err != nil || requests.Load() != n+1 {
		t.Fatal("actual single promotion refused/replayed", err, output)
	}
	var receipt api.ClusterPromotionReceipt
	if json.Unmarshal([]byte(output), &receipt) != nil || !receipt.Committed || receipt.MemberID != remote.Trust().MemberID() || receipt.Term.Epoch != image.Term.Epoch+1 {
		t.Fatal("missing actual durable receipt", output)
	}
	awaitServingRuntime(t, s, func() bool {
		lease, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		return lease.Bundle().Identity().Base.Term.Same(receipt.Term)
	})
	stop()
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}
	mirror = open()
	if !mirror.Authority().Term.Same(receipt.Term) || mirror.Authority().Mode != replication.Primary {
		t.Fatal("restart lost actual promoted authority")
	}
	audit, err := mirror.ReadAudit(finite(), sqlite.AuditQuery{Kind: "Cluster", Name: receipt.Term.ClusterID})
	if err != nil || len(audit) != 1 || audit[0].Action != "cluster.promote" || audit[0].Actor != "User:operator" || audit[0].RequestID != "reviewed-loss" || audit[0].Authz == "" {
		t.Fatal("missing attributed promotion audit", err, audit)
	}
	if err := mirror.VerifyAudit(finite()); err != nil {
		t.Fatal(err)
	}
	retained, err := memberidentity.Load(filepath.Join(dir, memberIdentityFile), receipt.MemberID)
	if err != nil || retained.Trust().Fingerprint() != remote.Trust().Fingerprint() {
		t.Fatal("promotion/restart replaced private CA", err)
	}
}

// This intentionally tests original cookie admission on a runtime without a
// mounted promotion coordinator. It grants no follower/promotion acceptance.
func TestClusterPromotionCookieCSRFBeforeBody(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	for _, csrf := range []string{"", info.CSRF} {
		bodyRead := false
		body := &clusterActivationBody{Reader: strings.NewReader(`{}`), before: func() { bodyRead = true }}
		req := httptest.NewRequest("POST", "/api/v1/cluster/promote", body)
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		if csrf != "" {
			req.Header.Set(auth.CSRFHeader, csrf)
		}
		result := httptest.NewRecorder()
		f.server.ServeHTTP(result, req)
		expected := http.StatusForbidden
		if csrf != "" {
			expected = http.StatusServiceUnavailable
		}
		if result.Code != expected || bodyRead {
			t.Fatal("cookie admission or missing capability acquired a body", result.Code, bodyRead)
		}
	}
}
