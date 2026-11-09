package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func clusterActivationReview(t *testing.T, f *servingHTTPFixture) api.ActivateClusterCommand {
	t.Helper()
	w := f.call(t, "GET", "/cluster/enrollment", "", f.token, nil, "")
	expectServingHTTPStatus(t, w, 200)
	var out api.ClusterEnrollmentInfo
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.Dormant || out.Mode != string(replication.Standalone) || !validClusterReview(out.ClusterEnrollmentReview) {
		t.Fatal("missing exact dormant review", err)
	}
	return api.ActivateClusterCommand{ClusterEnrollmentReview: out.ClusterEnrollmentReview, Name: "primary", Address: "127.0.0.1:9443", Reason: "initialize reviewed HA owner"}
}

func TestServingHTTPClusterActivationCommitted(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	in := clusterActivationReview(t, f)
	before := f.s.opts.Store.Authority()
	w := f.call(t, "POST", "/cluster/activate", roleBindingBody(t, in), f.token, nil, "")
	if w.Code != 200 {
		t.Fatalf("activation: %d %s", w.Code, w.Body.String())
	}
	var receipt api.ClusterActivationReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || !receipt.Committed || !receipt.RestartRequired || receipt.MemberID != in.MemberID || receipt.ClusterID != in.ClusterID {
		t.Fatal("missing actual commit/restart receipt", err)
	}
	a, owner, err := f.s.opts.Store.ReadMemberEnrollment(t.Context())
	if err != nil || a.Mode != replication.Primary || a.Incarnation != before.Incarnation+1 || owner.State() == nil {
		t.Fatal("activation not durably composed", err)
	}
	identity, err := memberidentity.Load(filepath.Join(f.s.opts.Config.Node.DataDir, memberIdentityFile), in.MemberID)
	if err != nil {
		t.Fatal("private identity not retained", err)
	}
	members := owner.State().Members()
	if len(members) != 1 || members[0].ID != in.MemberID || !bytes.Equal(members[0].Trust.Certificate(), identity.Trust().Certificate()) {
		t.Fatal("durable membership differs from local private identity")
	}
	entries, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Cluster", Name: in.ClusterID})
	if err != nil || len(entries) != 1 || entries[0].Action != "cluster.activate" || entries[0].Actor == "" || entries[0].Authz == "" || entries[0].Reason != in.Reason {
		t.Fatal("missing same-command attributed audit", err)
	}
	if err := f.s.opts.Store.VerifyAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestServingHTTPClusterActivationReviewedRefusals(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	in := clusterActivationReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	valid := roleBindingBody(t, in)
	for _, raw := range []string{`null`, valid + `{}`, strings.Replace(valid, `"name":"primary"`, `"name":"primary","name":"other"`, 1), strings.Replace(valid, `"name":"primary"`, `"extra":true,"name":"primary"`, 1)} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/cluster/activate", raw, f.token, nil, ""), 400)
	}
	for _, field := range []string{"source", "owner", "member", "cluster"} {
		q := in
		switch field {
		case "source":
			q.SourceDigest = strings.Repeat("a", 64)
		case "owner":
			q.OwnerDigest = strings.Repeat("a", 64)
		case "member":
			q.MemberID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		case "cluster":
			q.ClusterID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		}
		expectServingHTTPStatus(t, f.call(t, "POST", "/cluster/activate", roleBindingBody(t, q), f.token, nil, ""), 409)
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
	if _, err := os.Stat(filepath.Join(f.s.opts.Config.Node.DataDir, memberIdentityFile)); !os.IsNotExist(err) {
		t.Fatal("refused review created identity")
	}
}

func TestServingHTTPClusterActivationCanceledBeforeBody(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	in := clusterActivationReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/activate", strings.NewReader(roleBindingBody(t, in))).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+f.token)
	cancel()
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatal("canceled activation succeeded")
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
	if _, err := os.Stat(filepath.Join(f.s.opts.Config.Node.DataDir, memberIdentityFile)); !os.IsNotExist(err) {
		t.Fatal("canceled request created identity")
	}
}

func TestServingHTTPClusterActivationOriginalDeadline(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	deadline := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	called := false
	h := f.h.clusterEnrollmentLifetime(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		got, ok := r.Context().Deadline()
		if !ok || !got.Equal(deadline) {
			t.Error("activation extended original deadline")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/cluster/activate", nil).WithContext(ctx))
	if !called {
		t.Fatal("missing target admission")
	}
}

type clusterActivationBody struct {
	*strings.Reader
	before func()
}

func (b *clusterActivationBody) Read(p []byte) (int, error) {
	if b.before != nil {
		fn := b.before
		b.before = nil
		fn()
	}
	return b.Reader.Read(p)
}
func (*clusterActivationBody) Close() error { return nil }

func TestServingHTTPClusterActivationInterposedSource(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	in := clusterActivationReview(t, f)
	var after *replication.Image
	body := &clusterActivationBody{Reader: strings.NewReader(roleBindingBody(t, in)), before: func() {
		// This runs only after the real HTTP adapter captured its original lease.
		err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			id, ok := tokens.Parse(f.token)
			if !ok {
				t.Fatal("invalid fixture token")
			}
			return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: id}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true})
		})
		if err != nil {
			t.Fatal(err)
		}
		after = roleBindingImage(t, f.s.opts.Store)
	}}
	r := httptest.NewRequest("POST", "/api/v1/cluster/activate", nil)
	r.Body = body
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if after == nil {
		t.Fatal("source interposition never reached")
	}
	if w.Code < 400 {
		t.Fatal("activation used recaptured source or revoked proof")
	}
	roleBindingUnchanged(t, f.s.opts.Store, after)
	if _, err := os.Stat(filepath.Join(f.s.opts.Config.Node.DataDir, memberIdentityFile)); !os.IsNotExist(err) {
		t.Fatal("stale request created identity")
	}
}

func TestServingHTTPClusterActivationIdentityRefusalAndRollback(t *testing.T) {
	for _, invalidIdentity := range []bool{true, false} {
		name := "invalid-member"
		if invalidIdentity {
			name = "existing-corrupt-identity"
		}
		t.Run(name, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			f.s.opts.Config.Node.DataDir = t.TempDir()
			in := clusterActivationReview(t, f)
			path := filepath.Join(f.s.opts.Config.Node.DataDir, memberIdentityFile)
			if invalidIdentity {
				if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				in.Address = "not-a-host-port"
			}
			before := roleBindingImage(t, f.s.opts.Store)
			w := f.call(t, "POST", "/cluster/activate", roleBindingBody(t, in), f.token, nil, "")
			if w.Code < 400 {
				t.Fatal("invalid identity/target succeeded")
			}
			roleBindingUnchanged(t, f.s.opts.Store, before)
			entries, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Cluster", Name: in.ClusterID})
			if err != nil || len(entries) != 0 {
				t.Fatal("refusal committed activation audit", err)
			}
			if invalidIdentity {
				raw, err := os.ReadFile(path) //nolint:gosec // fixture-controlled identity bytes
				if err != nil || string(raw) != "corrupt" {
					t.Fatal("corrupt identity repaired", err)
				}
			} else {
				// Identity creation precedes staging and is not rolled back. A
				// new explicit valid request may retain this exact private CA.
				identity, err := memberidentity.Load(path, in.MemberID)
				if err != nil {
					t.Fatal("failed attempt identity not retained", err)
				}
				in.Address = "127.0.0.1:9443"
				expectServingHTTPStatus(t, f.call(t, "POST", "/cluster/activate", roleBindingBody(t, in), f.token, nil, ""), 200)
				_, owner, err := f.s.opts.Store.ReadMemberEnrollment(t.Context())
				if err != nil || !bytes.Equal(owner.State().Members()[0].Trust.Certificate(), identity.Trust().Certificate()) {
					t.Fatal("new explicit decision replaced identity", err)
				}
			}
		})
	}
}

func TestServingHTTPClusterActivationCookieCSRF(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	cookie, session := f.login(t)
	localUsersFresh(t, f)
	in := clusterActivationReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/cluster/activate", roleBindingBody(t, in), "", cookie, ""), 403)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	expectServingHTTPStatus(t, f.call(t, "POST", "/cluster/activate", roleBindingBody(t, in), "", cookie, session.CSRF), 200)
}

func TestServingHTTPClusterActivationAuditFailureRollsBack(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	in := clusterActivationReview(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	authority := f.s.opts.Store.Authority()
	db, err := sql.Open("sqlite", f.s.opts.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER refuse_activation_audit BEFORE INSERT ON audit WHEN NEW.action = 'cluster.activate' BEGIN SELECT RAISE(ABORT, 'activation-audit-test'); END`); err != nil {
		t.Fatal(err)
	}
	w := f.call(t, "POST", "/cluster/activate", roleBindingBody(t, in), f.token, nil, "")
	if w.Code < 400 {
		t.Fatal("audit refusal reported committed activation")
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
	if !reflect.DeepEqual(authority, f.s.opts.Store.Authority()) {
		t.Fatal("audit refusal changed authority")
	}
	identity, err := memberidentity.Load(filepath.Join(f.s.opts.Config.Node.DataDir, memberIdentityFile), in.MemberID)
	if err != nil || identity == nil {
		t.Fatal("test never reached post-identity staging", err)
	}
	entries, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Cluster", Name: in.ClusterID})
	if err != nil || len(entries) != 0 {
		t.Fatal("partial audit survived rollback", err)
	}
}
