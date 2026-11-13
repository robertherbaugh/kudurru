package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// recaptureProbe counts a current command's dispatch attempts, recaptures
// and transaction bodies (D399).
type recaptureProbe struct {
	attempts, recaptures, dispatches atomic.Int32
}

// supersedeCommand arms the runtime so that the next len(commits) dispatch
// attempts of a current command first commit commits[i] as another writer and
// wait until the attempt's captured publication is visibly superseded, so the
// attempt's first publication check refuses for exactly that reason.
func supersedeCommand(t *testing.T, s *servingRuntime, commits ...func(context.Context) error) *recaptureProbe {
	t.Helper()
	probe := &recaptureProbe{}
	s.commandHooks.Store(&commandHooks{
		beforeCheck: func(ctx context.Context) {
			n := int(probe.attempts.Add(1))
			if n > len(commits) {
				return
			}
			if err := commits[n-1](ctx); err != nil {
				t.Error("concurrent commit", err)
				return
			}
			status := s.primary.driver.Status()
			target := serving.TermActivation{Term: status.Writer.Term, Incarnation: status.Writer.Incarnation}
			for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(time.Millisecond) {
				if errors.Is(servinghttp.CheckWriter(ctx, s.publisher, target), serving.ErrPending) {
					return
				}
			}
			t.Error("concurrent commit did not supersede the captured publication")
		},
		afterRecapture: func(context.Context) { probe.recaptures.Add(1) },
		dispatch:       func(context.Context) { probe.dispatches.Add(1) },
	})
	t.Cleanup(func() { s.commandHooks.Store(nil) })
	return probe
}

// otherCommit is a commit by another writer that changes nothing the
// command depends on, like the zone lifecycle driver's status writes.
func otherCommit(s *servingRuntime, name string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := s.opts.Store.Apply(ctx, &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}
}

func (p *recaptureProbe) want(t *testing.T, attempts, recaptures, dispatches int32) {
	t.Helper()
	if p.attempts.Load() != attempts || p.recaptures.Load() != recaptures || p.dispatches.Load() != dispatches {
		t.Fatalf("attempts=%d recaptures=%d dispatches=%d, want %d %d %d", p.attempts.Load(), p.recaptures.Load(), p.dispatches.Load(), attempts, recaptures, dispatches)
	}
}

func auditRows(t *testing.T, s *servingRuntime, q sqlite.AuditQuery) []sqlite.AuditEntry {
	t.Helper()
	q.Limit = 1000
	rows, err := s.opts.Store.ReadAudit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func lastAuditID(t *testing.T, s *servingRuntime) int64 {
	t.Helper()
	rows := auditRows(t, s, sqlite.AuditQuery{Desc: true, Limit: 1})
	if len(rows) == 0 {
		return 0
	}
	return rows[0].ID
}

func sessionCount(t *testing.T, f *servingHTTPFixture, cookie *http.Cookie) int {
	t.Helper()
	w := f.call(t, "GET", "/auth/sessions", "", "", cookie, "")
	expectServingHTTPStatus(t, w, 200)
	var listed struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	return len(listed.Items)
}

// A sign-in whose captured publication another commit supersedes before its
// transaction body began captures again and signs in once: one session, the
// same audit rows as an undisturbed sign-in.
func TestServingCommandRecaptureSignInSupersededBeforeDispatch(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	before := lastAuditID(t, f.s)
	cookie, _ := f.login(t)
	control := len(auditRows(t, f.s, sqlite.AuditQuery{AfterID: before}))
	if sessionCount(t, f, cookie) != 1 {
		t.Fatal("control sign-in did not create one session")
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	before = lastAuditID(t, f.s)
	probe := supersedeCommand(t, f.s, otherCommit(f.s, "signin-concurrent"))
	second, info := f.login(t)
	probe.want(t, 2, 1, 1)
	f.s.commandHooks.Store(nil)
	if info.Principal.Name == "" || sessionCount(t, f, second) != 2 {
		t.Fatal("recaptured sign-in did not create exactly one more session")
	}
	// The concurrent commit and the sign-in's own rows; nothing twice.
	if rows := auditRows(t, f.s, sqlite.AuditQuery{AfterID: before}); len(rows) != control+1 {
		t.Fatalf("audit rows after a recaptured sign-in: %d, want %d", len(rows), control+1)
	}
	var policy public.Policy
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "signin-concurrent"}, &policy); err != nil {
		t.Fatal("concurrent commit lost", err)
	}
}

// An object write superseded the same way is applied once, with one audit
// row for its object.
func TestServingCommandRecaptureObjectWriteSupersededBeforeDispatch(t *testing.T) {
	f := newServingCommandFixture(t)
	srv := servingObjectAPI(t, f, nil)
	probe := supersedeCommand(t, f.s, otherCommit(f.s, "write-concurrent"))
	w := servingObjectRequest(t, f, srv, http.MethodPut, "/api/v1/objects/clientgroups/default/recaptured", servingObjectGroup("recaptured"), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("recaptured write: %d: %s", w.Code, w.Body)
	}
	probe.want(t, 2, 1, 1)
	rows := auditRows(t, f.s, sqlite.AuditQuery{Kind: "ClientGroup", Namespace: "default", Name: "recaptured"})
	if len(rows) != 1 {
		t.Fatalf("audit rows of the recaptured write: %d, want 1", len(rows))
	}
}

// expectRefusedOnce requires an effect-free 503 unavailable.
func expectRefusedOnce(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error api.ErrorBody `json:"error"`
	}
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Key != api.KeyUnavailable {
		t.Fatalf("want 503 unavailable, got %d: %s", w.Code, w.Body)
	}
}

// expectNoRetryAfter requires that a refusal from inside, at or after a
// command's transaction is not answered with Retry-After: only a refusal
// before any body began says that nothing was applied (D399).
func expectNoRetryAfter(t *testing.T, err error) {
	t.Helper()
	var e *api.Error
	if !errors.As(currentCommandAPIError(err), &e) || e.Status != http.StatusServiceUnavailable {
		t.Error("refusal not mapped to 503", err)
	} else if e.GetHeaders().Get("Retry-After") != "" {
		t.Error("a refusal after dispatch answered Retry-After")
	}
}

func clientGroupAbsent(t *testing.T, s *servingRuntime, name string) {
	t.Helper()
	var group public.ClientGroup
	if err := s.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &group); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused command was applied", err)
	}
}

// The cases D399 never retries each refuse once and apply nothing.
func TestServingCommandRecaptureRefusesWhatItMustNotRetry(t *testing.T) {
	t.Run("credential revoked by the superseding commit", func(t *testing.T) {
		f := newServingCommandFixture(t)
		srv := servingObjectAPI(t, f, nil)
		probe := supersedeCommand(t, f.s, func(ctx context.Context) error {
			tokens, err := f.s.opts.Store.List(ctx, "APIToken", store.ListOptions{})
			if err != nil {
				return err
			}
			for _, token := range tokens.Items {
				if err := f.s.opts.Store.Delete(ctx, store.Key{Kind: "APIToken", Name: token.GetName()}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
					return err
				}
			}
			return nil
		})
		w := servingObjectRequest(t, f, srv, http.MethodPut, "/api/v1/objects/clientgroups/default/revoked", servingObjectGroup("revoked"), "")
		expectRefusedOnce(t, w)
		probe.want(t, 1, 0, 0)
		clientGroupAbsent(t, f.s, "revoked")
	})
	t.Run("principal changed by the superseding commit", func(t *testing.T) {
		f := newServingHTTPFixture(t, false)
		cookie, info := f.login(t)
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		probe := supersedeCommand(t, f.s, func(ctx context.Context) error {
			var u internalv1.LocalUser
			if err := f.s.opts.Store.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &u); err != nil {
				return err
			}
			u.Spec.MustChangePassword = true
			_, err := f.s.opts.Store.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		w := f.call(t, "POST", "/auth/logout", "", "", cookie, info.CSRF)
		expectRefusedOnce(t, w)
		probe.want(t, 1, 0, 0)
		f.s.commandHooks.Store(nil)
		if sessionCount(t, f, cookie) != 1 {
			t.Fatal("a refused sign-out revoked its session")
		}
	})
	t.Run("cluster settings changed by the superseding commit", func(t *testing.T) {
		f := newServingCommandFixture(t)
		srv := servingObjectAPI(t, f, nil)
		probe := supersedeCommand(t, f.s, func(ctx context.Context) error {
			var settings public.Kudurru
			if err := f.s.opts.Store.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
				return err
			}
			settings.Labels = map[string]string{"recapture": "settings-changed"}
			_, err := f.s.opts.Store.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		w := servingObjectRequest(t, f, srv, http.MethodPut, "/api/v1/objects/clientgroups/default/settings-changed", servingObjectGroup("settings-changed"), "")
		expectRefusedOnce(t, w)
		probe.want(t, 1, 0, 0)
		clientGroupAbsent(t, f.s, "settings-changed")
	})
	t.Run("role bindings changed by the superseding commit", func(t *testing.T) {
		f := newServingCommandFixture(t)
		srv := servingObjectAPI(t, f, nil)
		probe := supersedeCommand(t, f.s, func(ctx context.Context) error {
			_, err := f.s.opts.Store.Apply(ctx, &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "recapture-binding", Namespace: "apps"}, Spec: public.RoleBindingSpec{Role: "viewer", Subjects: []public.BindingSubject{{Kind: "User", Name: "other"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		w := servingObjectRequest(t, f, srv, http.MethodPut, "/api/v1/objects/clientgroups/default/binding-changed", servingObjectGroup("binding-changed"), "")
		expectRefusedOnce(t, w)
		probe.want(t, 1, 0, 0)
		clientGroupAbsent(t, f.s, "binding-changed")
	})
	t.Run("a second command after the request's own commit", func(t *testing.T) {
		f := newServingCommandFixture(t)
		key := store.Key{Kind: "ClientGroup", Namespace: "default", Name: "own-commit"}
		probe := supersedeCommand(t, f.s)
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			write := func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
				if err := authorize(ctx, []rbac.Attributes{{Resource: "clientgroups", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Namespace: key.Namespace, Name: key.Name}}); err != nil {
					return err
				}
				group := servingObjectGroup(key.Name)
				group.Labels = map[string]string{"n": time.Now().Format("150405.000000000")}
				_, err := tx.Apply(ctx, group, store.ApplyOptions{Manager: store.ManagerAPI})
				return err
			}
			if err := f.s.ObjectCommand(r.Context(), key, write); err != nil {
				t.Error("first command", err)
			}
			awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
			if err := f.s.ObjectCommand(r.Context(), key, write); err == nil {
				t.Error("a request captured again after its own commit")
			}
		})).ServeHTTP(httptest.NewRecorder(), f.request())
		probe.want(t, 2, 0, 1)
	})
	t.Run("refused inside the transaction body", func(t *testing.T) {
		f := newServingCommandFixture(t)
		probe := supersedeCommand(t, f.s)
		var calls atomic.Int32
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			err := f.s.currentAuthCommand(r.Context(), func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error {
				calls.Add(1)
				return serving.ErrPending
			})
			if !errors.Is(err, serving.ErrPending) {
				t.Error("body refusal not returned", err)
			}
			expectNoRetryAfter(t, err)
		})).ServeHTTP(httptest.NewRecorder(), f.request())
		if calls.Load() != 1 {
			t.Fatal("a refusal from inside the transaction was repeated", calls.Load())
		}
		probe.want(t, 1, 0, 1)
	})
	t.Run("bound ends while every publication is superseded", func(t *testing.T) {
		f := newServingCommandFixture(t)
		srv := servingObjectAPI(t, f, nil)
		commits := make([]func(context.Context) error, 1000)
		var n atomic.Int32
		for i := range commits {
			commits[i] = func(ctx context.Context) error {
				_, err := f.s.opts.Store.Apply(ctx, &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "churn", Namespace: "default", Labels: map[string]string{"n": time.Now().Format("150405.000000000") + string(rune('a'+n.Add(1)%26))}}}, store.ApplyOptions{Manager: store.ManagerSystem})
				return err
			}
		}
		probe := supersedeCommand(t, f.s, commits...)
		began := time.Now()
		w := servingObjectRequest(t, f, srv, http.MethodPut, "/api/v1/objects/clientgroups/default/bounded", servingObjectGroup("bounded"), "")
		took := time.Since(began)
		expectRefusedOnce(t, w)
		t.Logf("refused after %s, %d attempts, %d recaptures, Retry-After %q", took.Round(time.Millisecond), probe.attempts.Load(), probe.recaptures.Load(), w.Header().Get("Retry-After"))
		if w.Header().Get("Retry-After") == "" || probe.dispatches.Load() != 0 || probe.recaptures.Load() < 1 || took < commandRecaptureWait || took > commandRecaptureWait+10*time.Second {
			t.Fatal("bounded recapture did not end at its bound without dispatch")
		}
		clientGroupAbsent(t, f.s, "bounded")
	})
}

// A command whose body began has dispatched: a refusal at its commit and an
// uncertain outcome after it (the commit landed, its publication was not
// acknowledged) are returned once and never repeated (D399).
func TestServingCommandRecaptureNeverRepeatsADispatchedCommand(t *testing.T) {
	t.Run("refused at commit", func(t *testing.T) {
		f := newServingCommandFixture(t)
		probe := supersedeCommand(t, f.s)
		var checks atomic.Int32
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			err := f.s.currentAuthCommand(r.Context(), func(_ context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
				return tx.RequireCommitCheck(func(context.Context) error {
					checks.Add(1)
					return serving.ErrPending
				})
			})
			if !errors.Is(err, serving.ErrPending) {
				t.Error("commit refusal not returned", err)
			}
			expectNoRetryAfter(t, err)
		})).ServeHTTP(httptest.NewRecorder(), f.request())
		if checks.Load() != 1 {
			t.Fatal("a command refused at commit was repeated", checks.Load())
		}
		probe.want(t, 1, 0, 1)
	})
	t.Run("uncertain outcome after commit", func(t *testing.T) {
		f := newServingCommandFixture(t)
		key := store.Key{Kind: "ClientGroup", Namespace: "default", Name: "uncertain"}
		probe := supersedeCommand(t, f.s)
		var bodies atomic.Int32
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			committed, err := f.s.currentAuthCommandThen(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
				bodies.Add(1)
				_, err := tx.Apply(ctx, servingObjectGroup(key.Name), store.ApplyOptions{Manager: store.ManagerAPI})
				return err
			}, func(context.Context, serving.TermActivation) error { return serving.ErrPending })
			if !committed || !errors.Is(err, serving.ErrPending) {
				t.Error("uncertain outcome not reported as committed", committed, err)
			}
			expectNoRetryAfter(t, err)
		})).ServeHTTP(httptest.NewRecorder(), f.request())
		if bodies.Load() != 1 {
			t.Fatal("a committed command was repeated", bodies.Load())
		}
		probe.want(t, 1, 0, 1)
		if rows := auditRows(t, f.s, sqlite.AuditQuery{Kind: key.Kind, Namespace: key.Namespace, Name: key.Name}); len(rows) > 1 {
			t.Fatalf("audit rows of an uncertain command: %d", len(rows))
		}
	})
}

// An integration operation's first no-op holds the integration host's lease
// barrier and its runtime lock: a superseded publication refuses it at once,
// without a recapture wait under those locks.
func TestServingCommandRecaptureIntegrationOperationRefusesAtOnce(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	probe := supersedeCommand(t, f.s, otherCommit(f.s, "integration-concurrent"))
	r := httptest.NewRequest(http.MethodPost, "https://kudurru.test/api/v1/integration-operation", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	called := false
	var took time.Duration
	f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		began := time.Now()
		op, err := f.s.integrations.beginOperation(r.Context(), integrationOwner(f.s, f.d), f.d.ResourceVersion, "check")
		took = time.Since(began)
		if err == nil {
			op.close()
			t.Error("a superseded integration operation was admitted")
		} else if !errors.Is(err, serving.ErrPending) {
			t.Error("superseded integration operation:", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("request did not reach the operation")
	}
	probe.want(t, 1, 0, 0)
	if took >= commandRecaptureWait {
		t.Fatal("the refusal waited for a recapture", took)
	}
}

// issueJoinTokenOnce sends one reviewed join-token command.
func issueJoinTokenOnce(t *testing.T, f *servingHTTPFixture, in api.ClusterJoinTokenCommand) *joinTokenWriter {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/cluster/join-token", strings.NewReader(roleBindingBody(t, in)))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	w := &joinTokenWriter{ResponseRecorder: httptest.NewRecorder()}
	f.server.ServeHTTP(w, r)
	return w
}

// A join-token command compares its reviewed source digest only outside its
// transaction, so it captures again only on the same source generation
// (WithExactSource): a commit that changed the source refuses it once, a
// republication of the same source admits it once.
func TestServingCommandRecaptureJoinTokenExactSource(t *testing.T) {
	t.Run("source changed by the superseding commit", func(t *testing.T) {
		f := newJoinTokenFixture(t)
		in := joinTokenReview(t, f)
		probe := supersedeCommand(t, f.s, otherCommit(f.s, "join-concurrent"))
		w := issueJoinTokenOnce(t, f, in)
		f.s.commandHooks.Store(nil)
		expectRefusedOnce(t, w.ResponseRecorder)
		probe.want(t, 1, 0, 0)
		if audit, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Cluster", Name: in.ClusterID}); err != nil || len(audit) != 0 {
			t.Fatal("a refused join-token command was audited", err)
		}
		if _, owner, err := f.s.opts.Store.ReadMemberEnrollment(t.Context()); err != nil || owner.Digest() != in.OwnerDigest {
			t.Fatal("a refused join-token command changed enrollment", err)
		}
	})
	t.Run("same source published again", func(t *testing.T) {
		f := newJoinTokenFixture(t)
		in := joinTokenReview(t, f)
		probe := supersedeCommand(t, f.s, func(context.Context) error { f.s.publisher.Wake(); return nil })
		w := issueJoinTokenOnce(t, f, in)
		f.s.commandHooks.Store(nil)
		if w.Code != http.StatusOK {
			t.Fatalf("join-token on a republished source: %d %s", w.Code, w.Body)
		}
		probe.want(t, 2, 1, 1)
		var receipt api.ClusterJoinTokenReceipt
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		checkJoinToken(t, f, in, receipt) // one invitation, one audit row
	})
}
