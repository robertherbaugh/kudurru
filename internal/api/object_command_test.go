package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/runtime"
)

func objectCommandHarness(t *testing.T, authorize ObjectAuthorize) (*harness, *sqlite.Store) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "objects.db"), sqlite.Options{Scheme: scheme, Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if authorize == nil {
		authorize = func(context.Context, []rbac.Attributes) error { return nil }
	}
	// Deliberately keep Options.Store pointing at the harness's unrelated
	// memory store: command reads/writes must use the actual SQLite cut.
	h := newHarness(t, func(opts *Options) {
		opts.ObjectCommand = func(ctx context.Context, _ store.Key, fn func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error {
			return st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return fn(ctx, tx, authorize)
			})
		}
	})
	return h, st
}

func commandRecordDoc(zone string) map[string]any {
	return map[string]any{"kind": "DNSRecordSet", "metadata": map[string]any{"name": "www", "namespace": "default"}, "spec": map[string]any{
		"zoneRef": map[string]any{"name": zone}, "endpoints": []any{map[string]any{"dnsName": "www", "recordType": "A", "recordTTL": int64(60), "targets": []any{"192.0.2.1"}}},
	}}
}

func TestObjectCommandUsesCurrentStateForBothMoveScopes(t *testing.T) {
	var scopes []string
	deny := false
	h, st := objectCommandHarness(t, func(_ context.Context, attrs []rbac.Attributes) error {
		scopes = nil
		for _, attr := range attrs {
			scopes = append(scopes, attr.Scope.Name)
		}
		if deny {
			return errForbidden(Missing{Resource: "records", Verb: "write", Scope: "zone:private"}, "")
		}
		return nil
	})
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/default/www", "admin", commandRecordDoc("private")).want(http.StatusCreated)
	// This false old scope must never authorize the actual current object.
	h.seed(store.ManagerAPI, recordSetObj("default", "www", "public", ep("www", "A", "192.0.2.1")))
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deny = true
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/default/www", "admin", commandRecordDoc("next")).want(http.StatusForbidden)
	if !reflect.DeepEqual(scopes, []string{"next", "private"}) {
		t.Fatal("write used a stale or incomplete old/new scope", scopes)
	}
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("denied move mutated objects", err)
	}
}

func TestObjectCommandCRUDOwnershipETagAndZoneReferences(t *testing.T) {
	h, st := objectCommandHarness(t, nil)
	path := "/api/v1/objects/dnszones/example"
	r := h.do(http.MethodPut, path, "admin", zoneDoc("example", "example.test.")).want(http.StatusCreated)
	rv := r.Header().Get("ETag")
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r = h.do(http.MethodPut, path, "admin", zoneDoc("example", "example.test.")).want(http.StatusOK)
	after, err := st.ReadGeneration(t.Context())
	if err != nil || rv != r.Header().Get("ETag") || !before.Generation().Same(after.Generation()) {
		t.Fatal("unchanged command changed RV/generation", err)
	}
	h.do(http.MethodPut, path, "admin", zoneDoc("example", "example.test."), "If-Match", "stale").want(http.StatusPreconditionFailed)
	h.do(http.MethodDelete, path, "admin", nil, "If-Match", "stale").want(http.StatusPreconditionFailed)
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/default/www", "admin", commandRecordDoc("example")).want(http.StatusCreated)
	// The unrelated live Store is empty; only a transaction read sees this ref.
	h.do(http.MethodDelete, path, "admin", nil, "If-Match", rv).want(http.StatusConflict)
	h.do(http.MethodDelete, "/api/v1/objects/dnsrecordsets/default/www", "admin", nil).want(http.StatusNoContent)
	h.do(http.MethodDelete, path, "admin", nil, "If-Match", rv).want(http.StatusNoContent)
	h.do(http.MethodDelete, path, "admin", nil).want(http.StatusNotFound)
	for _, owner := range []string{store.ManagerBootstrap, store.ManagerFile("zones.yaml")} {
		z := zoneObj("managed", "managed.test.")
		if _, err := st.Apply(t.Context(), z, store.ApplyOptions{Manager: owner, Force: true}); err != nil {
			t.Fatal(err)
		}
		want := http.StatusOK
		if owner != store.ManagerBootstrap {
			want = http.StatusConflict
		}
		h.do(http.MethodPut, "/api/v1/objects/dnszones/managed", "admin", zoneDoc("managed", "managed.test.")).want(want)
		var got public.DNSZone
		if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "managed"}, &got); err != nil {
			t.Fatal(err)
		}
		if owner == store.ManagerBootstrap && store.ManagedBy(&got) != store.ManagerAPI {
			t.Fatal("bootstrap takeover lost manager")
		}
	}
}

func TestObjectCommandRefusalNeverFallsBack(t *testing.T) {
	h, st := objectCommandHarness(t, nil)
	h.srv.opts.ObjectCommand = func(context.Context, store.Key, func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error {
		return errUnavailable("original command fenced")
	}
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.test.")).want(http.StatusServiceUnavailable)
	var got public.DNSZone
	key := store.Key{Kind: "DNSZone", Name: "example"}
	if !errors.Is(st.Get(t.Context(), key, &got), store.ErrNotFound) || !errors.Is(h.st.Get(t.Context(), key, &got), store.ErrNotFound) {
		t.Fatal("refusal fell back to an independent write")
	}
}

func TestObjectCommandPolicyLimitUnderActualLicensePin(t *testing.T) {
	h, st := objectCommandHarness(t, nil)
	manager, err := license.New(license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.opts.License = manager
	k, _ := kindByKind("Policy")
	write := func(name string) error {
		doc := map[string]any{"kind": "Policy", "metadata": map[string]any{"name": name, "namespace": "default"}, "spec": map[string]any{}}
		return manager.WithCurrent(t.Context(), func(license.Cut) error {
			_, err := h.srv.writeObject(t.Context(), writeRequest{kind: k, body: doc})
			return err
		})
	}
	if err := write("other"); err == nil {
		t.Fatal("community command allowed nondefault Policy")
	}
	if err := write("default"); err != nil {
		t.Fatal("default Policy create failed under license pin", err)
	}
	if _, err := st.Apply(t.Context(), &public.Policy{ObjectMeta: objectMeta("extra", "default")}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if err := write("default"); err == nil {
		t.Fatal("policy count used the unrelated empty live Store")
	}
}
