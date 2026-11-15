package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingObjectAPI(t *testing.T, f *servingCommandFixture, command api.ObjectCommand) *api.Server {
	t.Helper()
	if command == nil {
		command = f.s.ObjectCommand
	}
	srv, err := api.New(api.Options{
		Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		ObjectCommand: command, Authenticate: f.http.Middleware, Authorizer: f.http,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func servingObjectRequest(t *testing.T, f *servingCommandFixture, srv *api.Server, method, path string, body any, match string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "http://kudurru.test"+path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	if match != "" {
		r.Header.Set("If-Match", match)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func servingObjectGroup(name string) *public.ClientGroup {
	return &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.10"}}}}
}

func TestServingObjectCommandActualAPIAndCompleteSQLite(t *testing.T) {
	f := newServingCommandFixture(t)
	srv := servingObjectAPI(t, f, nil)
	path := "/api/v1/objects/clientgroups/default/objects-test"
	write := func(method string, body any, match string, code int) *httptest.ResponseRecorder {
		t.Helper()
		w := servingObjectRequest(t, f, srv, method, path, body, match)
		if w.Code != code {
			t.Fatalf("%s: %d, want %d: %s", method, w.Code, code, w.Body)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		return w
	}
	group := servingObjectGroup("objects-test")
	w := write(http.MethodPut, group, "", http.StatusCreated)
	rv := w.Header().Get("ETag")
	if rv == "" {
		t.Fatal("committed API object has no ETag")
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w = write(http.MethodPut, group, rv, http.StatusOK)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) || rv != w.Header().Get("ETag") {
		t.Fatal("actual authenticated no-op changed generation/RV", err)
	}
	group.Spec.Clients[0].IP = "192.0.2.11"
	write(http.MethodPut, group, "stale", http.StatusPreconditionFailed)
	w = write(http.MethodPut, group, rv, http.StatusOK)
	write(http.MethodDelete, nil, rv, http.StatusPreconditionFailed)
	write(http.MethodDelete, nil, w.Header().Get("ETag"), http.StatusNoContent)
	var deleted public.ClientGroup
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: group.Name}, &deleted), store.ErrNotFound) {
		t.Fatal("delete did not commit to actual SQLite")
	}
}

func TestServingObjectCommandRejectsMissingRepeatedAndForeignAdmission(t *testing.T) {
	f := newServingCommandFixture(t)
	key := store.Key{Kind: "ClientGroup", Namespace: "default", Name: "guarded"}
	attrs := []rbac.Attributes{{Resource: "clientgroups", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Namespace: key.Namespace, Name: key.Name}}
	for _, mode := range []string{"missing", "repeated", "wrong-name", "wrong-resource"} {
		before, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		called := false
		var retained api.ObjectAuthorize
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			err := f.s.ObjectCommand(r.Context(), key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
				called, retained = true, authorize
				if mode == "missing" {
					return nil // Even a successful no-op must authorize.
				}
				want := append([]rbac.Attributes(nil), attrs...)
				if mode == "wrong-name" {
					want[0].Name = "another-target"
				}
				if mode == "wrong-resource" {
					want[0].Resource = "settings"
				}
				if err := authorize(ctx, want); err != nil {
					return nil // Ignored refusal must still poison this command.
				}
				if mode == "repeated" {
					_ = authorize(ctx, want)
				}
				_, err := tx.Apply(ctx, servingObjectGroup(key.Name), store.ApplyOptions{Manager: store.ManagerAPI})
				return err
			})
			if err == nil {
				t.Errorf("%s admission accepted", mode)
			}
		})).ServeHTTP(httptest.NewRecorder(), f.request())
		if !called || retained == nil || retained(t.Context(), attrs) == nil {
			t.Fatal("callback was not reached or retained authority remained usable", mode)
		}
		after, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil || !before.Generation().Same(after.Generation()) {
			t.Fatal("invalid admission changed objects", mode, err)
		}
	}
	for _, kind := range []store.Kind{"IdentityProvider", "RoleBinding", "APIToken", "ZoneState", "Unknown"} {
		called := false
		if err := f.s.ObjectCommand(t.Context(), store.Key{Kind: kind, Namespace: "default", Name: "refused"}, func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error { called = true; return nil }); err == nil || called {
			t.Fatal("unsupported source/private mutation entered callback", kind, err)
		}
	}
}

func TestServingObjectCommandCurrentOldAndNewRecordScopes(t *testing.T) {
	f := newServingCommandFixture(t)
	id, _ := tokens.Parse(f.token)
	setScopes := func(scopes []string) {
		t.Helper()
		var token internalv1.APIToken
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
			t.Fatal(err)
		}
		token.Spec.Scopes = scopes
		if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	}
	setScopes([]string{"zone-admin@zone:new"})
	rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "moving", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "old"}, Endpoints: []public.Endpoint{{DNSName: "www", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.1"}}}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), rs, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	srv := servingObjectAPI(t, f, nil)
	rs.Spec.ZoneRef.Name = "new"
	path := "/api/v1/objects/dnsrecordsets/default/moving"
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w := servingObjectRequest(t, f, srv, http.MethodPut, path, rs, "")
	if w.Code != http.StatusForbidden {
		t.Fatal("new-zone-only token moved a record from old zone", w.Code, w.Body)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("denied old-zone scope changed durable objects", err)
	}
	setScopes([]string{"zone-admin@zone:new", "zone-admin@zone:old"})
	w = servingObjectRequest(t, f, srv, http.MethodPut, path, rs, "")
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("immutable")) {
		t.Fatal("authorized move did not preserve zoneRef immutability", w.Code, w.Body)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	// A scoped zone-admin may update an existing zone, but creation is cluster.
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "new"}, Spec: public.DNSZoneSpec{ZoneName: "new.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.new.test."}}}
	w = servingObjectRequest(t, f, srv, http.MethodPut, "/api/v1/objects/dnszones/new", zone, "")
	if w.Code != http.StatusForbidden {
		t.Fatal("zone-scoped token created new cluster authority", w.Code, w.Body)
	}
}

func TestServingObjectCommandOriginalExpiryAndWriterAtFinal(t *testing.T) {
	for _, mode := range []string{"expiry-write", "expiry-noop", "writer-write", "writer-noop"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			key := store.Key{Kind: "ClientGroup", Namespace: "default", Name: "final"}
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				err := f.s.ObjectCommand(r.Context(), key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
					called = true
					if err := authorize(ctx, []rbac.Attributes{{Resource: "clientgroups", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Namespace: key.Namespace, Name: key.Name}}); err != nil {
						return err
					}
					if mode == "expiry-write" || mode == "writer-write" {
						if _, err := tx.Apply(ctx, servingObjectGroup(key.Name), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
							return err
						}
					}
					if mode == "expiry-write" || mode == "expiry-noop" {
						f.offset.Store(int64(f.expires.Sub(f.now))) // exact boundary
					} else {
						f.cancel()
					}
					return nil
				})
				if err == nil {
					t.Error("original lifetime ended but mutation/no-op succeeded")
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if !called {
				t.Fatal("original authenticated command was not reached")
			}
			f.stop()
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("final expiry/cancellation escaped rollback", err)
			}
		})
	}
}

func TestServingObjectCommandReleasedAndStaleHTTPLease(t *testing.T) {
	f := newServingCommandFixture(t)
	key := store.Key{Kind: "ClientGroup", Namespace: "default", Name: "stale"}
	var retained context.Context
	callback := func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
		if err := authorize(ctx, []rbac.Attributes{{Resource: "clientgroups", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Namespace: key.Namespace, Name: key.Name}}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, servingObjectGroup(key.Name), store.ApplyOptions{Manager: store.ManagerAPI})
		return err
	}
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		retained = r.Context()
		if err := f.s.ObjectCommand(retained, key, callback); err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		if err := f.s.ObjectCommand(retained, key, callback); err == nil {
			t.Fatal("old lease borrowed newly published view for no-op")
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if retained == nil || f.s.ObjectCommand(retained, key, callback) == nil {
		t.Fatal("released original lease remained writable")
	}
}

func TestServingObjectCommandAPINoopAndDeleteCannotSkipFinal(t *testing.T) {
	for _, operation := range []string{"update", "unchanged", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newServingCommandFixture(t)
			group := servingObjectGroup("api-final")
			if _, err := f.s.opts.Store.Apply(t.Context(), group, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			command := func(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
				return f.s.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
					if err := fn(ctx, tx, authorize); err != nil {
						return err
					}
					called = true
					// Expire only after the actual route has selected its result.
					f.offset.Store(int64(f.expires.Sub(f.now)))
					return nil
				})
			}
			srv := servingObjectAPI(t, f, command)
			method := http.MethodPut
			var body any = group
			switch operation {
			case "update":
				group.Spec.Clients[0].IP = "192.0.2.11"
			case "delete":
				method, body = http.MethodDelete, nil
			}
			w := servingObjectRequest(t, f, srv, method, "/api/v1/objects/clientgroups/default/api-final", body, "")
			if !called || w.Code != http.StatusUnauthorized {
				t.Fatal("route bypassed final original credential expiry", w.Code, w.Body)
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("API mutation/no-op committed after final expiry", err)
			}
		})
	}
}
