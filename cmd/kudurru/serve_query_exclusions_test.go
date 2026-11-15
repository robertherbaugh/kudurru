package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunServeQueryExclusionsOriginalLists(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "proof", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{
			{Name: "excluded", Format: "domains", Inline: []string{"proof.test"}, Exclude: []string{"proof.test", "stale.test"}},
			{Name: "active", Format: "domains", Inline: []string{"proof.test"}},
		}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"proof"}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
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
				t.Error("query exclusion mount shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("query exclusion mount did not join")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("query exclusion mount did not become ready")
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
	var result api.DNSQueryResult
	controlMountedRequest(t, client, base, http.MethodPost, "/dns/query", login.CSRF, api.DNSQueryRequest{Name: "proof.test", Type: "A"}, http.StatusOK, &result)
	if result.StatusClass != "blocked" || result.List != "default/proof" || len(result.Exclusions) != 1 || result.ExclusionsTruncated {
		t.Fatal("mounted query lost its selected source exclusions or final block decision")
	}
	witness := result.Exclusions[0]
	if witness.List != result.List || witness.Rule != "proof.test" || witness.Source != 0 || witness.Effect != "block" {
		t.Fatal("mounted exclusion provenance changed")
	}
}
