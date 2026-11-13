package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
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
