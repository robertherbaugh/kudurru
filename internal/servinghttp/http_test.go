package servinghttp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fixture struct {
	store               *sqlite.Store
	publisher           *serving.Publisher
	adapter             *Adapter
	deps                servingauth.Dependencies
	row                 servingauth.SessionRecord
	cookie, token, csrf string
	pause               atomic.Bool
	released            atomic.Int64
	now                 time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Now().UTC(), csrf: "csrf-secret-value-longer-than-thirty-two-bytes"}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	f.store, err = sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: private, ValidateReplicaEntry: servingauth.ValidateEntry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	var tokenID string
	f.token, tokenID = tokens.Generate()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := security.Initialize(ctx, tx); err != nil {
			return err
		}
		created := metav1.NewTime(f.now.Add(-time.Hour))
		u, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{Groups: []string{"staff"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		for _, object := range []store.Object{
			&publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			&publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "default"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "staff"}}}},
			&internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: tokenID, CreationTimestamp: created}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(f.token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: "alice", UID: string(u.GetUID()), ID: "local:alice", Provider: "local"}, Scopes: []string{"viewer"}}},
		} {
			if _, err := tx.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		s, err := security.Load(ctx, tx)
		if err != nil {
			return err
		}
		f.row, err = s.Enroll(servingauth.SessionRecord{ID: "http-session-012345678901234", Account: servingauth.AccountRef{Kind: "LocalUser", Name: "alice", UID: string(u.GetUID())}, Provider: "local", IssuedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Hour), IdleExpiresAt: f.now.Add(30 * time.Minute), Grants: []string{"viewer"}, CSRFHash: servingauth.CSRFHash(f.csrf)})
		if err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	f.deps = servingauth.Dependencies{Identity: "http-test", ExpectedIdentity: "http-test", Permissions: permissions, License: servingauth.LicensePolicy{RBAC: true}, CurrentKey: bytes.Repeat([]byte{4}, 32)}
	initial, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registry := servingauth.Schema{Scheme: scheme, Kinds: initial.KindManifest, SchemaVersion: store.SchemaVersion}
	f.publisher, err = serving.New(serving.Options{Source: f.store, Required: serving.CompleteManifest(), RetryInterval: time.Millisecond, Prepare: func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
		if f.pause.Load() {
			return serving.Prepared{}, serving.ErrPrepare
		}
		cut, err := servingauth.CaptureImage(ctx, c.Image, registry)
		if err != nil {
			return serving.Prepared{}, err
		}
		view, err := servingauth.Prepare(ctx, cut, f.deps)
		if err != nil {
			return serving.Prepared{}, err
		}
		p := serving.Prepared{Current: func() bool { return true }}
		for _, name := range serving.CompleteManifest() {
			// Only auth is under test. Other owner values are explicit fixtures,
			// never a production publisher composition.
			component := serving.Component{Name: name, Base: c.Token.Generation(), Identity: c.Image.PayloadSHA256, Value: struct{}{}}
			if name == "auth" {
				component.Value = view
				component.Identity = view.DependencyIdentity()
				component.Release = func() { view.Release(); f.released.Add(1) }
			}
			p.Components = append(p.Components, component)
		}
		return p, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.publisher.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("publisher failed to stop")
		}
	})
	select {
	case <-f.publisher.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not initialize")
	}
	f.adapter, err = New(Options{Publisher: f.publisher, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	value, _ := lease.Bundle().Component("auth")
	f.cookie, err = value.(*servingauth.View).SealSession(f.row.ID, f.csrf)
	lease.Release()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) command(t *testing.T, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := f.store.Command(t.Context(), f.store.Authority().Term, fn); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) request(method string) *http.Request {
	r := httptest.NewRequest(method, "http://kudurru.test/api/v1/zones", nil)
	r.AddCookie(&http.Cookie{Name: session.DefaultCookieName, Value: f.cookie}) //nolint:gosec // Request cookie, no response attributes.
	return r
}
func readAttrs() rbac.Attributes {
	return rbac.Attributes{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
}
func (f *fixture) waitCurrent(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		changed := f.publisher.Subscribe()
		lease, err := f.publisher.CaptureHTTP(ctx)
		if err == nil {
			lease.Release()
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("publisher did not catch up")
		}
	}
}

func TestHTTPHeldCutAndCurrentMutationUseDifferentAuthority(t *testing.T) {
	f := newFixture(t)
	entered := make(chan context.Context, 1)
	resume := make(chan struct{})
	done := make(chan error, 1)
	h := f.adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		<-resume
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p.Name != "alice" {
			done <- errors.New("missing captured principal")
			return
		}
		identity, err := Identity(r.Context())
		if err != nil || !identity.Base.Term.Same(f.store.Authority().Term) {
			done <- errors.New("lost captured identity")
			return
		}
		d, err := f.adapter.Authorize(r.Context(), rbac.Principal{BuiltinAdmin: true}, readAttrs())
		if err != nil || !d.Allowed {
			done <- errors.New("held read mixed generations")
			return
		}
		d, err = f.adapter.Authorize(r.Context(), rbac.Principal{BuiltinAdmin: true}, rbac.Attributes{Resource: "settings", Verb: "write"})
		if err != nil || d.Allowed {
			done <- errors.New("caller principal replaced captured authority")
			return
		}
		proof, err := Credential(r.Context())
		if err != nil {
			done <- err
			return
		}
		err = f.store.Command(r.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, _, err := servingauth.ReauthorizeCurrent(ctx, tx, f.deps, proof, readAttrs(), f.now)
			return err
		})
		if !errors.Is(err, servingauth.ErrCredentials) {
			done <- errors.New("held read authorized a revoked current command")
			return
		}
		w.WriteHeader(http.StatusOK)
		done <- nil
	}))
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { h.ServeHTTP(response, f.request(http.MethodGet)); close(finished) }()
	var held context.Context
	select {
	case held = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not capture")
	}
	f.pause.Store(true)
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := security.Load(ctx, tx)
		if err != nil {
			return err
		}
		if err = s.Revoke(f.row.ID); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	reject := httptest.NewRecorder()
	f.adapter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("pending generation reached handler") })).ServeHTTP(reject, f.request(http.MethodGet))
	if reject.Code != http.StatusServiceUnavailable {
		t.Fatal("pending generation failed open", reject.Code)
	}
	f.pause.Store(false)
	f.publisher.Wake()
	f.waitCurrent(t)
	reject = httptest.NewRecorder()
	f.adapter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("revoked session reached handler") })).ServeHTTP(reject, f.request(http.MethodGet))
	if reject.Code != http.StatusUnauthorized {
		t.Fatal("new request retained revoked session", reject.Code)
	}
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request blocked")
	}
	<-finished
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if _, err := Credential(held); err == nil {
		t.Fatal("raw proof escaped lease lifetime")
	}
	if _, err := Component[*servingauth.View](held, "auth"); err == nil {
		t.Fatal("view escaped lease lifetime")
	}
	if f.released.Load() == 0 {
		t.Fatal("retired owner remained leased")
	}
}

func TestHTTPCookieCSRFAndBearerSelection(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		name      string
		method    string
		configure func(*http.Request)
		status    int
	}{
		{"read", http.MethodGet, nil, 200},
		{"missing-csrf", http.MethodPost, nil, 403},
		{"valid-csrf", http.MethodPost, func(r *http.Request) { r.Header.Set(auth.CSRFHeader, f.csrf) }, 200},
		{"cross-origin", http.MethodPost, func(r *http.Request) {
			r.Header.Set(auth.CSRFHeader, f.csrf)
			r.Header.Set("Origin", "http://evil.test")
		}, 403},
		{"bearer-wins", http.MethodPost, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }, 200},
		{"bad-bearer-no-cookie-fallback", http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer kdr_wrong") }, 401},
		{"unsupported-bearer", http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer jwt-unrecognized") }, 401},
		{"duplicate-header", http.MethodGet, func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+f.token)
			r.Header.Add("Authorization", "Bearer "+f.token)
		}, 401},
		{"duplicate-cookie", http.MethodGet, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: session.DefaultCookieName, Value: f.cookie}) }, 401}, //nolint:gosec // Request cookie.
		{"no-proof", http.MethodGet, func(r *http.Request) { r.Header.Del("Cookie") }, 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := f.request(test.method)
			if test.configure != nil {
				test.configure(r)
			}
			out := httptest.NewRecorder()
			f.adapter.Middleware(auth.Require(f.adapter, func(*http.Request) rbac.Attributes { return readAttrs() }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proof, err := Credential(r.Context())
				if err != nil {
					t.Error(err)
				}
				if proof.Kind == servingauth.Cookie {
					if _, ok := auth.SessionFrom(r.Context()); !ok {
						t.Error("missing same-cut CSRF session")
					}
				}
				w.WriteHeader(http.StatusOK)
			}))).ServeHTTP(out, r)
			if out.Code != test.status {
				t.Fatalf("status %d, want %d", out.Code, test.status)
			}
		})
	}
}

func TestHTTPDeadlineAndHandlerPanicReleaseLeases(t *testing.T) {
	f := newFixture(t)
	a, err := New(Options{Publisher: f.publisher, MaxDuration: 10 * time.Millisecond, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	var held context.Context
	a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { held = r.Context(); <-held.Done() })).ServeHTTP(httptest.NewRecorder(), f.request(http.MethodGet))
	if !errors.Is(held.Err(), context.DeadlineExceeded) {
		t.Fatal("request was unbounded")
	}
	if _, err := Identity(held); err == nil {
		t.Fatal("expired context retained serving access")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("handler did not panic")
			}
		}()
		f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { held = r.Context(); panic("handler") })).ServeHTTP(httptest.NewRecorder(), f.request(http.MethodGet))
	}()
	if _, err := Credential(held); err == nil {
		t.Fatal("panic retained request proof")
	}
}
