package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/downloadpolicy"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunServeHTTPSOnlyDownloads(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.AllowLocalListURLs = true
	cfg.Blocking.HTTPSOnly = true
	cfg.Blocking.AllowDegraded = true
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer origin.Close()
	cfg.Blocking.Catalogue.URL = origin.URL + "/catalogue"
	cfg.Blocking.Catalogue.AdGuardServicesURL = origin.URL + "/services"
	// Both actual startup catalogue constructors must carry the host policy.
	refreshers, err := newCatalogueRefreshers(cfg, catalogue.NewHolder(nil), cluster.Self{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*catalogue.Refresher{refreshers.document, refreshers.adguard} {
		r.Refresh(t.Context())
		if r.Status().LastError != downloadpolicy.ErrHTTPSRequired.Error() {
			t.Fatal("startup catalogue lost HTTPS-only policy")
		}
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	obj := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "https-required", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "remote", URL: origin.URL + "/list", Format: "domains"}}}}
	if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// Complete publications require the exact last-good digest. Seed that
	// historical cache explicitly: enabling a download policy neither certifies
	// it anew nor permits a missing source to become an empty filtering list.
	cache, err := listfetcher.OpenDiskCache(filepath.Join(cfg.Node.DataDir, "lists"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := cache.Put(origin.URL+"/list", strings.NewReader("cached.example\n"), listfetcher.PutOptions{Fetched: time.Now().Add(-48 * time.Hour), Origin: listfetcher.OriginFetch})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: obj.Name}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Status.Sources = []public.SourceStatus{{Name: "remote", Digest: entry.Digest}}
	if err := st.UpdateStatus(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("download-policy mount shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("download-policy mount did not join")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("download-policy mount did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	deadline = time.Now().Add(5 * time.Second)
	for {
		var observed public.BlockList
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/blocklists/default/https-required", "", nil, http.StatusOK, &observed)
		if len(observed.Status.Sources) == 1 && observed.Status.Sources[0].Error == downloadpolicy.ErrHTTPSRequired.Error() {
			if observed.Status.Sources[0].Digest != entry.Digest {
				t.Fatal("refused refresh lost the published cached digest")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mounted BlockList did not publish HTTPS-only refusal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	controlMountedRequest(t, client, base, http.MethodPatch, "/node/config", login.CSRF, map[string]any{"blocking": map[string]any{"httpsOnly": false}}, http.StatusBadRequest, nil)
	if hits.Load() != 0 {
		t.Fatal("HTTPS-only host contacted a plaintext origin")
	}
}
