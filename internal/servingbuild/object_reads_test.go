package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type heldObjectReads struct {
	*ObjectReads
	remaining atomic.Int64
	entered   chan context.Context
	resume    chan struct{}
}

func (h *heldObjectReads) ObjectSnapshot(ctx context.Context) (zonecache.ObjectSet, replication.Generation, error) {
	if h.remaining.Add(-1) >= 0 {
		h.entered <- ctx
		select {
		case <-h.resume:
		case <-ctx.Done():
			return zonecache.ObjectSet{}, replication.Generation{}, ctx.Err()
		}
	}
	return h.ObjectReads.ObjectSnapshot(ctx)
}

func objectReadHTTP(t *testing.T, f *fixture, source apihttp.ObjectReadSource) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host, Authenticate: auth.Middleware, Authorizer: auth, ObjectReads: source, CrossOrigin: cop})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

type objectReadResponse struct {
	diagnosticResponse
	etag string
}

func objectReadGET(ctx context.Context, server *httptest.Server, token, path string) objectReadResponse {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/"+path, nil)
	if err != nil {
		return objectReadResponse{diagnosticResponse: diagnosticResponse{err: err}}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		return objectReadResponse{diagnosticResponse: diagnosticResponse{err: err}}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return objectReadResponse{diagnosticResponse: diagnosticResponse{code: response.StatusCode, body: string(body), err: err}, etag: response.Header.Get("ETag")}
}

func objectReadWait(t *testing.T, h *heldObjectReads) context.Context {
	t.Helper()
	select {
	case ctx := <-h.entered:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach object read after original auth capture")
		return nil
	}
}

func cleanupHeldObjectReads(t *testing.T, h *heldObjectReads) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-h.resume:
		default:
			close(h.resume)
		}
	})
}

func objectReadPolicy(name, marker string) *public.Policy {
	return &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"marker": marker}}, Spec: public.PolicySpec{BlockListRefs: []string{"deny"}}}
}

func TestObjectReadsActualSQLiteHeldCreateEditDeleteRecreate(t *testing.T) {
	for _, change := range []string{"create", "edit", "delete", "recreate"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			key := store.Key{Kind: "Policy", Namespace: "default", Name: "subject"}
			var old store.Object
			if change != "create" {
				var err error
				old, err = f.store.Apply(t.Context(), objectReadPolicy(key.Name, "original"), store.ApplyOptions{Manager: store.ManagerSystem})
				if err != nil {
					t.Fatal(err)
				}
				f.waitCurrent(t)
			}
			h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 4), resume: make(chan struct{})}
			paths := []string{"objects/policies", "policies", "objects/policies/default/subject", "policies/default/subject"}
			h.remaining.Store(int64(len(paths)))
			server := objectReadHTTP(t, f, h)
			cleanupHeldObjectReads(t, h)
			results := make([]chan objectReadResponse, len(paths))
			for i, path := range paths {
				results[i] = make(chan objectReadResponse, 1)
				go func() { results[i] <- objectReadGET(t.Context(), server, f.token, path) }()
			}
			contexts := make([]context.Context, len(paths))
			for i := range paths {
				contexts[i] = objectReadWait(t, h)
				objects, _, err := h.ObjectReads.ObjectSnapshot(contexts[i])
				if err != nil {
					t.Fatal(err)
				}
				_, exists := objects.Get(key)
				if exists != (change != "create") {
					t.Fatal("original captured object presence", exists)
				}
			}
			switch change {
			case "delete", "recreate":
				if err := f.store.Delete(t.Context(), key, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
			}
			var current store.Object
			if change != "delete" {
				var err error
				current, err = f.store.Apply(t.Context(), objectReadPolicy(key.Name, "successor"), store.ApplyOptions{Manager: store.ManagerSystem})
				if err != nil {
					t.Fatal(err)
				}
				if change == "recreate" && current.GetUID() == old.GetUID() {
					t.Fatal("fixture failed to recreate UID")
				}
			}
			f.waitCurrent(t)
			close(h.resume)
			for i, path := range paths {
				result := <-results[i]
				code := http.StatusOK
				if change == "create" && i >= 2 {
					code = http.StatusNotFound
				}
				body := diagnosticWant(t, result.diagnosticResponse, code)
				if strings.Contains(body, "successor") || (change != "create" && !strings.Contains(body, "original")) {
					t.Fatalf("%s mixed later live source after %s: %s", path, change, body)
				}
				if i >= 2 && old != nil && result.etag != old.GetResourceVersion() {
					t.Fatal("held detail lost original ETag", result.etag, old.GetResourceVersion())
				}
				if old != nil && !strings.Contains(body, string(old.GetUID())) {
					t.Fatal("held response lost original source UID", body)
				}
			}
			for _, ctx := range contexts {
				if _, _, err := h.ObjectReads.ObjectSnapshot(ctx); err == nil {
					t.Fatal("completed request allowed a released/canceled original lease")
				}
			}
			fresh := objectReadGET(t.Context(), server, f.token, "objects/policies/default/subject")
			if change == "delete" {
				diagnosticWant(t, fresh.diagnosticResponse, http.StatusNotFound)
			} else {
				body := diagnosticWant(t, fresh.diagnosticResponse, http.StatusOK)
				if !strings.Contains(body, "successor") || fresh.etag != current.GetResourceVersion() {
					t.Fatal("fresh request did not see actual successor desired cut", body, fresh.etag)
				}
				if change == "recreate" && !strings.Contains(body, string(current.GetUID())) {
					t.Fatal("fresh request used old UID")
				}
			}
		})
	}
}

func TestObjectReadsActualSQLitePaginationHeldAndFresh(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"page-a", "page-b", "page-c"} {
		p := objectReadPolicy(name, "page")
		if _, err := f.store.Apply(t.Context(), p, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	f.waitCurrent(t)
	h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
	server := objectReadHTTP(t, f, h)
	cleanupHeldObjectReads(t, h)
	var first apihttp.ObjectList
	body := diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "objects/policies?limit=1&labelSelector=marker=page").diagnosticResponse, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &first); err != nil || first.Continue == "" {
		t.Fatal("first page", body, err)
	}
	h.remaining.Store(1)
	path := "objects/policies?limit=1&labelSelector=marker==page&continue=" + first.Continue
	done := make(chan objectReadResponse, 1)
	go func() { done <- objectReadGET(t.Context(), server, f.token, path) }()
	ctx := objectReadWait(t, h)
	_, original, err := h.ObjectReads.ObjectSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Apply(t.Context(), objectReadPolicy("page-aa", "page"), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	close(h.resume)
	var second apihttp.ObjectList
	body = diagnosticWant(t, (<-done).diagnosticResponse, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &second); err != nil || len(second.Items) != 1 || second.Items[0]["metadata"].(map[string]any)["name"] != "page-b" || second.Continue == "" {
		t.Fatal("held second page crossed publication", body, err)
	}
	lease, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Bundle().Identity().Base.Same(original) {
		t.Fatal("fixture did not publish changed generation")
	}
	lease.Release()
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, path).diagnosticResponse, http.StatusConflict)
	body = diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "policies?labelSelector=marker=page").diagnosticResponse, http.StatusOK)
	if !strings.Contains(body, "page-aa") {
		t.Fatal("fresh alias did not see new member", body)
	}
}

func TestObjectReadsActualSQLiteHeldRecordZoneRefAuthorization(t *testing.T) {
	f := newFixture(t)
	role := &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "default"}, Spec: public.RoleBindingSpec{
		Role: "zone-admin", Scope: public.BindingScope{Kind: "zone", Name: "uncommitted"}, Subjects: []public.BindingSubject{{Kind: "Group", Name: "staff"}},
	}}
	foreign := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "foreign"}, Spec: public.DNSZoneSpec{ZoneName: "foreign.example.", Type: public.ZoneTypePrimary}}
	row := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "default"}, Spec: public.DNSRecordSetSpec{
		ZoneRef: public.ObjectRef{Name: "uncommitted"}, Endpoints: []public.Endpoint{{DNSName: "www.uncommitted.example", RecordType: "A", Targets: []string{"192.0.2.1"}}},
	}}
	for _, obj := range []store.Object{role, foreign, row} {
		if _, err := f.store.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	f.waitCurrent(t)
	h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 2), resume: make(chan struct{})}
	h.remaining.Store(2)
	server := objectReadHTTP(t, f, h)
	cleanupHeldObjectReads(t, h)
	paths := []string{"objects/dnsrecordsets/default/subject", "objects/dnsrecordsets?limit=1"}
	results := make([]chan objectReadResponse, len(paths))
	for i, path := range paths {
		results[i] = make(chan objectReadResponse, 1)
		go func() { results[i] <- objectReadGET(t.Context(), server, f.token, path) }()
	}
	for range paths {
		objectReadWait(t, h)
	}
	row.Spec.ZoneRef.Name = "foreign"
	row.Spec.Endpoints[0].DNSName = "www.foreign.example"
	// zoneRef is immutable for a UID; actual source recreation is how this
	// key can acquire a different authorization identity.
	if err := f.store.Delete(t.Context(), store.KeyOf(row), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Apply(t.Context(), row, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	close(h.resume)
	for _, result := range results {
		body := diagnosticWant(t, (<-result).diagnosticResponse, http.StatusOK)
		if !strings.Contains(body, "www.uncommitted.example") || strings.Contains(body, "www.foreign.example") {
			t.Fatal("held authorization used successor zoneRef", body)
		}
	}
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, paths[0]).diagnosticResponse, http.StatusForbidden)
	body := diagnosticWant(t, objectReadGET(t.Context(), server, f.token, paths[1]).diagnosticResponse, http.StatusOK)
	var page apihttp.ObjectList
	if err := json.Unmarshal([]byte(body), &page); err != nil || len(page.Items) != 0 || page.Continue != "" {
		t.Fatal("successor list leaked unauthorized record/cursor", body, err)
	}
}

func TestObjectReadsOnlyOriginalRequestAndNoRecapture(t *testing.T) {
	r := NewObjectReads()
	for _, ctx := range []context.Context{nil, context.Background()} {
		if _, _, err := r.ObjectSnapshot(ctx); err == nil {
			t.Fatal("missing original request accepted")
		}
	}
	var zero *ObjectReads
	if _, _, err := zero.ObjectSnapshot(t.Context()); err == nil {
		t.Fatal("nil adapter accepted")
	}
	f := newFixture(t)
	auth, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}})
	if err != nil {
		t.Fatal(err)
	}
	var original context.Context
	handler := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		original = request.Context()
		if _, _, err := r.ObjectSnapshot(original); err != nil {
			t.Fatal("valid original owner rejected", err)
		}
		ctx, cancel := context.WithCancel(original)
		cancel()
		if _, _, err := r.ObjectSnapshot(ctx); !errors.Is(err, serving.ErrReleased) {
			t.Fatal("canceled original accepted", err)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if original == nil {
		t.Fatal("request did not capture")
	}
	if _, _, err := r.ObjectSnapshot(original); !errors.Is(err, serving.ErrReleased) {
		t.Fatal("released lease triggered recapture", err)
	}
}

func TestObjectReadsRejectsIncompleteAndWrongGenerationOwners(t *testing.T) {
	for _, malformed := range []string{"type", "missing-registry", "incomplete-registry", "revision"} {
		t.Run(malformed, func(t *testing.T) {
			f := newFixture(t)
			publisher, err := serving.New(serving.Options{Source: f.store, Required: serving.CompleteManifest(), Prepare: func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
				prepared, err := f.builder.Prepare(ctx, c)
				if err != nil {
					return prepared, err
				}
				for i := range prepared.Components {
					component := &prepared.Components[i]
					if component.Name != "objects" {
						continue
					}
					objects := *component.Value.(*zonecache.Snapshot)
					switch malformed {
					case "type":
						component.Value = struct{}{}
					case "missing-registry":
						objects.Objects = zonecache.ObjectSet{}
						component.Value = &objects
					case "incomplete-registry":
						partial, prepareErr := zonecache.Prepare(zonecache.PrepareOptions{Generation: objects.Generation, Kinds: []store.Kind{"Policy"}}, nil, nil)
						if prepareErr != nil {
							return prepared, prepareErr
						}
						component.Value = partial
					case "revision":
						objects.Generation++
						component.Value = &objects
					}
				}
				return prepared, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); _ = publisher.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done; publisher.Close() })
			select {
			case <-publisher.Initialized():
			case <-time.After(5 * time.Second):
				t.Fatal("malformed-owner fixture failed to publish")
			}
			auth, err := servinghttp.New(servinghttp.Options{Publisher: publisher, Cookie: session.CookieConfig{Secure: session.SecureAlways}})
			if err != nil {
				t.Fatal(err)
			}
			called := false
			handler := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				if _, _, err := NewObjectReads().ObjectSnapshot(r.Context()); !errors.Is(err, serving.ErrComponents) {
					t.Error("malformed owner accepted", malformed, err)
				}
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+f.token)
			handler.ServeHTTP(httptest.NewRecorder(), req)
			if !called {
				t.Fatal("request did not reach malformed object owner")
			}
		})
	}
}
