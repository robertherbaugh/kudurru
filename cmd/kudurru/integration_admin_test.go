package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
)

func integrationAdminRouter(t *testing.T, f *integrationOperationFixture) http.Handler {
	t.Helper()
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, CanonicalSettings: f.s, Integrations: f.s.integrations, Authenticate: f.adapter.Middleware, Authorizer: f.adapter, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func integrationAdminCall(t *testing.T, router http.Handler, f *integrationOperationFixture, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "https://kudurru.test/api/v1"+path, bytes.NewReader(payload))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	for _, private := range []string{f.path, "test-only-private-credential", "credentialFingerprints", "providerIDs", "receiptID"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("private integration material reached response")
		}
	}
	return w
}

func TestIntegrationAdminActualRouterReadCheckAndRefusals(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	router := integrationAdminRouter(t, f)
	path := "/integrations/default/manual"
	check := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion}
	if w := integrationAdminCall(t, router, f, http.MethodGet, "/integrations?namespace=default", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatal("anonymous integration read", w.Code)
	}
	w := integrationAdminCall(t, router, f, http.MethodGet, path, nil, f.token)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("original integration read", w.Code)
	}
	var read api.IntegrationSummary
	if err := json.Unmarshal(w.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.UID != check.UID || read.Revision != check.Revision || read.Type != "unifi" {
		t.Fatal("read lost exact reviewed identity")
	}
	before := f.calls.Load()
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/check", check, f.token)
	if w.Code != http.StatusOK {
		t.Fatal("real check failed", w.Code)
	}
	var result api.IntegrationExecutionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || !result.Reachable || !result.Authenticated || result.Uncertain || f.calls.Load() != before+1 {
		t.Fatal("check acknowledgment lost original outcome")
	}
	// Wait for the preceding operation's budget publication before a distinct
	// request. This does not retry a command or refresh the reviewed identity.
	awaitServingRuntime(t, f.s, func() bool {
		l, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer l.Release()
		base, current := l.Bundle().Identity().Base, f.s.opts.Store.Authority().Installed
		return base.Term.Same(current.Term) && base.Revision == current.Revision
	})
	// The UID/revision never refreshes implicitly and a refusal never sends I/O.
	check.UID = "a-different-uid"
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/check", check, f.token)
	if w.Code != http.StatusConflict || f.calls.Load() != before+1 {
		t.Fatal("stale review reached provider", w.Code)
	}
}

func TestIntegrationAdminActualRouterObserverSync(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	router := integrationAdminRouter(t, f)
	path := "/integrations/default/manual"
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion, Mode: "observe"}
	w := integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token)
	if w.Code != http.StatusOK {
		t.Fatal("source sync failed", w.Code)
	}
	var result api.IntegrationExecutionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || !result.SourceCommitted || !result.SourcePublished || result.Uncertain {
		t.Fatal("source outcome was not acknowledged")
	}
}

func TestIntegrationAdminActualRouterUnadvertisedActions(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	router := integrationAdminRouter(t, f)
	path := "/integrations/default/manual"
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion}
	w := integrationAdminCall(t, router, f, http.MethodGet, path+"/actions?uid="+review.UID+"&revision="+review.Revision, nil, f.token)
	if w.Code != http.StatusOK {
		t.Fatal("advertised action read", w.Code)
	}
	var result api.IntegrationExecutionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || len(result.Actions) != 0 {
		t.Fatal("invented provider action")
	}
	before := f.calls.Load()
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/actions/reboot", review, f.token)
	if w.Code != http.StatusNotImplemented || f.calls.Load() != before {
		t.Fatal("unadvertised action reached provider", w.Code)
	}
}
