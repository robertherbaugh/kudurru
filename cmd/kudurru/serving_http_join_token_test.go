package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func newJoinTokenFixture(t *testing.T) *servingHTTPFixture {
	t.Helper()
	f := &servingHTTPFixture{servingCommandFixture: newServingCommandFixture(t, func(s *servingRuntime) { enrollServingMemberFixture(t, s) })}
	f.clock.Set(time.Now)
	var err error
	f.h, err = newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	f.server, err = api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, CanonicalSettings: f.s, Authenticate: f.h.Middleware, AuthHandler: f.h.Handler(), Authorizer: f.h.Authorizer(), Roles: f.h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func joinTokenReview(t *testing.T, f *servingHTTPFixture) api.ClusterJoinTokenCommand {
	t.Helper()
	w := f.call(t, "GET", "/cluster/enrollment", "", f.token, nil, "")
	expectServingHTTPStatus(t, w, 200)
	var out api.ClusterEnrollmentInfo
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Dormant || out.Mode != string(replication.Primary) {
		t.Fatal("missing enrolled primary review")
	}
	return api.ClusterJoinTokenCommand{ClusterEnrollmentReview: out.ClusterEnrollmentReview, Name: "new-member", Address: "127.0.0.3:9443", ExpiresAt: time.Now().Add(time.Minute).UTC().Truncate(time.Second), Reason: "reviewed enrollment"}
}

func checkJoinToken(t *testing.T, f *servingHTTPFixture, in api.ClusterJoinTokenCommand, out api.ClusterJoinTokenReceipt) {
	t.Helper()
	if !out.Committed || out.Invitation == "" || !out.ExpiresAt.Equal(in.ExpiresAt) {
		t.Fatal("missing committed invitation receipt")
	}
	inv, err := enrollment.ParseInvitation(out.Invitation)
	if err != nil {
		t.Fatal("invalid invitation")
	}
	name, address := inv.Target()
	issuer, _, trust := inv.IssuingPeer()
	if name != in.Name || address != in.Address || issuer.MemberID != in.MemberID || trust.MemberID() != in.MemberID {
		t.Fatal("reviewed target/issuer changed")
	}
	_, owner, err := f.s.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := memberidentity.Create(filepath.Join(t.TempDir(), "remote.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := inv.Propose(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.State().Accept(proposal, issuer, time.Now()); err != nil {
		t.Fatal("issued secret does not match committed hash", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(out.Invitation, "kudurru-join-v1."))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Secret == "" {
		t.Fatal("fixture secret missing")
	}
	image, err := json.Marshal(roleBindingImage(t, f.s.opts.Store))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(image), wire.Secret) || strings.Contains(string(image), out.Invitation) {
		t.Fatal("bearer secret persisted in canonical image")
	}
	audit, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Cluster", Name: in.ClusterID})
	if err != nil || len(audit) != 1 || audit[0].Action != "cluster.join-token" || audit[0].Actor == "" || audit[0].Authz == "" || audit[0].Reason != in.Reason {
		t.Fatal("missing attributed issuance audit", err)
	}
	if err := f.s.opts.Store.VerifyAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestClusterJoinTokenActualHTTP(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "http1", true: "http2"}[http2], func(t *testing.T) {
			f := newJoinTokenFixture(t)
			in := joinTokenReview(t, f)
			srv := httptest.NewUnstartedServer(f.server)
			srv.EnableHTTP2 = http2
			srv.StartTLS()
			defer srv.Close()
			c, err := apiclient.New(srv.URL, apiclient.Options{HTTPClient: srv.Client(), Token: f.token})
			if err != nil {
				t.Fatal(err)
			}
			var out api.ClusterJoinTokenReceipt
			if err := c.IssueClusterJoinToken(t.Context(), in, &out); err != nil {
				t.Fatal("actual issuance refused", err)
			}
			checkJoinToken(t, f, in, out)
		})
	}
}

// Recorder transport boundaries are explicit here; real HTTP/1 and HTTP/2
// tests above exercise net/http's actual deadline and flush implementations.
type joinTokenWriter struct {
	*httptest.ResponseRecorder
	read, write time.Time
	failFlush   bool
}

func (w *joinTokenWriter) SetReadDeadline(at time.Time) error  { w.read = at; return nil }
func (w *joinTokenWriter) SetWriteDeadline(at time.Time) error { w.write = at; return nil }
func (w *joinTokenWriter) FlushError() error {
	if w.failFlush {
		return errors.New("fixture flush failure")
	}
	w.Flush()
	return nil
}

func TestClusterJoinTokenSourceCapturedBeforeBody(t *testing.T) {
	f := newJoinTokenFixture(t)
	in := joinTokenReview(t, f)
	interposed := false
	body := &clusterActivationBody{Reader: strings.NewReader(roleBindingBody(t, in)), before: func() {
		interposed = true
		writer, err := f.s.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.opts.Store.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
			return tx.PutSection(ctx, "dedup", replication.Entry{Key: "interposed", Data: []byte(`{}`)})
		}); err != nil {
			t.Fatal(err)
		}
	}}
	r := httptest.NewRequest("POST", "/api/v1/cluster/join-token", nil)
	r.Body = body
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	w := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder()}
	f.server.ServeHTTP(w, r)
	if !interposed || w.Code < 400 || strings.Contains(w.Body.String(), "kudurru-join-v1.") {
		t.Fatal("late source rescued original command")
	}
	_, owner, err := f.s.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil || owner.Digest() != in.OwnerDigest {
		t.Fatal("refused operation changed owner", err)
	}
}

func TestClusterJoinTokenFlushFailureRetainsCommit(t *testing.T) {
	f := newJoinTokenFixture(t)
	in := joinTokenReview(t, f)
	r := httptest.NewRequest("POST", "/api/v1/cluster/join-token", strings.NewReader(roleBindingBody(t, in)))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	w := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder(), failFlush: true}
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Error("failed output did not abort handler")
			}
		}()
		f.server.ServeHTTP(w, r)
	}()
	_, owner, err := f.s.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil || owner.Digest() == in.OwnerDigest {
		t.Fatal("output failure claimed rollback", err)
	}
	var receipt api.ClusterJoinTokenReceipt
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal("response was appended after failed flush")
	}
	checkJoinToken(t, f, in, receipt)
}

func TestClusterJoinTokenCLIActualRouter(t *testing.T) {
	f := newJoinTokenFixture(t)
	in := joinTokenReview(t, f)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); f.server.ServeHTTP(w, r) }))
	defer srv.Close()
	run := func(args ...string) (string, error) {
		var out strings.Builder
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", srv.URL, "--token", f.token, "-o", "json", "cluster", "join-token"}, args...))
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	if _, err := run(); err == nil || requests.Load() != 0 {
		t.Fatal("missing review performed I/O")
	}
	args := []string{"--cluster-id", in.ClusterID, "--member-id", in.MemberID, "--source-digest", in.SourceDigest, "--owner-digest", in.OwnerDigest, "--name", in.Name, "--address", in.Address, "--expires-at", in.ExpiresAt.Format(time.RFC3339), "--reason", in.Reason}
	output, err := run(args...)
	if err != nil || requests.Load() != 1 {
		t.Fatal("issuance failed or repeated", err)
	}
	var out api.ClusterJoinTokenReceipt
	if json.Unmarshal([]byte(output), &out) != nil {
		t.Fatal("invalid CLI receipt")
	}
	checkJoinToken(t, f, in, out)
}

func TestClusterJoinTokenReviewedRefusals(t *testing.T) {
	f := newJoinTokenFixture(t)
	in := joinTokenReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	for _, mode := range []string{"owner", "source", "expired", "long-expiry", "target", "duplicate", "unknown-field", "anonymous"} {
		t.Run(mode, func(t *testing.T) {
			q := in
			switch mode {
			case "owner":
				q.OwnerDigest = strings.Repeat("a", 64)
			case "source":
				q.SourceDigest = strings.Repeat("a", 64)
			case "expired":
				q.ExpiresAt = time.Now().Add(-time.Minute)
			case "long-expiry":
				q.ExpiresAt = time.Now().Add(time.Hour)
			case "target":
				q.Address = "https://foreign.example"
			}
			raw := roleBindingBody(t, q)
			if mode == "duplicate" {
				raw = strings.Replace(raw, `"name":"new-member"`, `"name":"new-member","name":"another"`, 1)
			}
			if mode == "unknown-field" {
				raw = strings.Replace(raw, `"name":"new-member"`, `"unknown":true,"name":"new-member"`, 1)
			}
			r := httptest.NewRequest("POST", "/api/v1/cluster/join-token", strings.NewReader(raw))
			r.Header.Set("Content-Type", "application/json")
			if mode != "anonymous" {
				r.Header.Set("Authorization", "Bearer "+f.token)
			}
			w := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder()}
			f.server.ServeHTTP(w, r)
			if w.Code < 400 || strings.Contains(w.Body.String(), "kudurru-join-v1.") {
				t.Fatal("invalid command issued a secret", mode, w.Code)
			}
			roleBindingUnchanged(t, f.s.opts.Store, before)
		})
	}
}

func TestClusterJoinTokenDeadlineCannotBeExtendedOrCleared(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	base := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder()}
	w := &clusterBoundedWriter{ResponseWriter: base, ctx: ctx, deadline: deadline}
	for _, at := range []time.Time{deadline.Add(time.Hour), {}, deadline} {
		if w.SetReadDeadline(at) != nil || w.SetWriteDeadline(at) != nil || base.read.After(deadline) || base.write.After(deadline) {
			t.Fatal("original deadline extended")
		}
	}
	cancel()
	if w.SetReadDeadline(time.Time{}) != nil || w.SetWriteDeadline(time.Time{}) != nil || time.Now().Before(base.read) || time.Now().Before(base.write) {
		t.Fatal("cancellation interrupt undone")
	}
}

func TestClusterJoinTokenAuditFailureRollsBack(t *testing.T) {
	f := newJoinTokenFixture(t)
	in := joinTokenReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	db, err := sql.Open("sqlite", f.s.opts.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER refuse_invitation_audit BEFORE INSERT ON audit WHEN NEW.action='cluster.join-token' BEGIN SELECT RAISE(ABORT,'invitation-audit-test'); END`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/cluster/join-token", strings.NewReader(roleBindingBody(t, in)))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	w := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder()}
	f.server.ServeHTTP(w, r)
	if w.Code < 400 || strings.Contains(w.Body.String(), "kudurru-join-v1.") {
		t.Fatal("audit failure emitted invitation")
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
}

func TestClusterJoinTokenCookieCSRF(t *testing.T) {
	f := newJoinTokenFixture(t)
	hash, err := f.h.opts.Hasher.Hash(t.Context(), "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Store.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	localUsersFresh(t, f)
	cookie, session := f.login(t)
	localUsersFresh(t, f)
	in := joinTokenReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	srv := httptest.NewServer(f.server)
	defer srv.Close()
	for _, csrf := range []string{"", session.CSRF} {
		request, err := http.NewRequestWithContext(t.Context(), "POST", srv.URL+"/api/v1/cluster/join-token", strings.NewReader(roleBindingBody(t, in)))
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(cookie)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Kudurru-CSRF", csrf)
		response, err := srv.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if csrf == "" {
			_ = response.Body.Close()
			if response.StatusCode != 403 {
				t.Fatal("cookie missing CSRF accepted", response.StatusCode)
			}
			roleBindingUnchanged(t, f.s.opts.Store, before)
			continue
		}
		var out api.ClusterJoinTokenReceipt
		err = json.NewDecoder(response.Body).Decode(&out)
		_ = response.Body.Close()
		if response.StatusCode != 200 || err != nil {
			t.Fatal("original cookie/CSRF refused", response.StatusCode, err)
		}
		checkJoinToken(t, f, in, out)
	}
}

func TestClusterJoinTokenSlowBodyOriginalDeadline(t *testing.T) {
	f := newJoinTokenFixture(t)
	in := joinTokenReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 150*time.Millisecond)
		defer cancel()
		f.server.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Keep the declared body incomplete on a real socket. Huma's later
	// relative deadline/clear cannot extend the original 150ms request.
	raw := roleBindingBody(t, in)
	request := "POST /api/v1/cluster/join-token HTTP/1.1\r\nHost: kudurru.test\r\nAuthorization: Bearer " + f.token + "\r\nContent-Type: application/json\r\nContent-Length: 64000\r\n\r\n" + raw[:len(raw)/2]
	start := time.Now()
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if response != nil {
		_ = response.Body.Close()
		if response.StatusCode < 400 {
			t.Fatal("partial body accepted")
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("body read outlived original deadline", err)
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
}
