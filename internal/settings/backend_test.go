package settings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func TestKubeBackendCompareAndSwap(t *testing.T) {
	var current *corev1.ConfigMap
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if current == nil {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
				return
			}
			_ = json.NewEncoder(w).Encode(current)
		case http.MethodPost, http.MethodPut:
			var cm corev1.ConfigMap
			_ = json.NewDecoder(r.Body).Decode(&cm)
			if current != nil && (r.Method == http.MethodPost || cm.ResourceVersion != current.ResourceVersion) {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
				return
			}
			if current == nil {
				cm.ResourceVersion = "1"
			} else {
				cm.ResourceVersion = "2"
			}
			current = &cm
			_ = json.NewEncoder(w).Encode(current)
		}
	}))
	defer server.Close()
	client, err := coreclient.NewForConfig(&rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	b := KubeBackend{Maps: client.ConfigMaps("release"), Name: "test-ui-settings"}
	_, rev, err := b.Read(t.Context())
	if err != nil || rev != "" {
		t.Fatalf("missing: %q %v", rev, err)
	}
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Owners: map[string]string{"upstreams": UI}, Values: map[string]any{"upstreams": upstreamValue(cfg)}}
	first, err := b.Save(t.Context(), doc, rev)
	if err != nil || first != "1" {
		t.Fatalf("create: %q %v", first, err)
	}
	if _, err = b.Save(t.Context(), doc, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent create: %v", err)
	}
	if _, err = b.Save(t.Context(), doc, "0"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write: %v", err)
	}
	if _, err = b.Save(t.Context(), doc, first); err != nil {
		t.Fatal(err)
	}
	got, rev, err := b.Read(t.Context())
	if err != nil || rev != "2" || got.Owners["upstreams"] != UI {
		t.Fatalf("read: %+v %s %v", got, rev, err)
	}
}

func TestStartupCacheAndFailures(t *testing.T) {
	backend := &memoryBackend{}
	cfg := newManager(t, backend, "", []string{"KUDURRU_STORE__DRIVER=kube"})
	if err := cfg.Transition(t.Context(), "upstreams", "adopt", revision(cfg)); err != nil {
		t.Fatal(err)
	}
	cached := CachedBackend{Backend: backend, Path: filepath.Join(t.TempDir(), "cache.json")}
	if _, _, err := cached.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend.failure = apierrors.NewServiceUnavailable("offline")
	if _, _, err := cached.Read(t.Context()); err == nil {
		t.Fatal("regular read silently used cache")
	}
	doc, _, err := ReadStartup(t.Context(), cached)
	if err != nil || doc.Owners["upstreams"] != UI {
		t.Fatalf("startup cache: %+v %v", doc, err)
	}
	backend.failure = apierrors.NewForbidden(corev1.Resource("configmaps"), "settings", errors.New("denied"))
	if _, _, err := cached.ReadStartup(t.Context()); err == nil {
		t.Fatal("permission failure silently used cache")
	}
	backend.failure = context.DeadlineExceeded
	if err := os.WriteFile(cached.Path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cached.ReadStartup(t.Context()); err == nil {
		t.Fatal("corrupt cache ignored")
	}
}
