package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestIntegrationAdminActualUniFiTLSObservation(t *testing.T) {
	controller, calls, _ := integrationController(t)
	f := &integrationOperationFixture{}
	f.s, f.path = integrationFixture(t, nil, nil)
	integrationOperationCaller(t, f)
	d := integrationPinnedDefinition(t, controller, f.path)
	d.Spec.UniFi.Groups.ByNetwork = nil
	integrationOperationStart(t, f, d)
	before := calls.Load()
	router := integrationAdminRouter(t, f)
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion, Mode: "observe"}
	w := integrationAdminCall(t, router, f, http.MethodPost, "/integrations/default/inventory/sync", review, f.token)
	if w.Code != http.StatusOK {
		t.Fatal("actual UniFi source operation refused", w.Code)
	}
	var result api.IntegrationExecutionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || !result.SourceCommitted || !result.SourcePublished || result.Clients != 1 || calls.Load() <= before {
		t.Fatal("actual UniFi acquisition/publication not acknowledged")
	}
}

func TestIntegrationAdminActualCookieCSRF(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	csrf := "private-csrf-fixture-value-at-least-32-bytes"
	id := "integration-session-012345678901234"
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "operation-admin"}, &user); err != nil {
			return err
		}
		state, err := security.Load(ctx, tx)
		if err != nil {
			return err
		}
		now := time.Now()
		_, err = state.Enroll(servingauth.SessionRecord{ID: id, Account: servingauth.AccountRef{Kind: "LocalUser", Name: user.Name, UID: string(user.UID)}, Provider: "local", IssuedAt: now, ExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(30 * time.Minute), Grants: []string{"admin"}, CSRFHash: servingauth.CSRFHash(csrf)})
		if err != nil {
			return err
		}
		return state.Commit(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	var sealed string
	awaitServingHTTPView(t, f.s, func(view *servingauth.View) bool { sealed, err = view.SealSession(id, csrf); return err == nil })
	router := integrationAdminRouter(t, f)
	body, err := json.Marshal(api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	for _, proof := range []string{"", csrf} {
		request := httptest.NewRequest(http.MethodPost, "https://kudurru.test/api/v1/integrations/default/manual/check", strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Cookie", session.DefaultCookieName+"="+sealed)
		request.Header.Set(auth.CSRFHeader, proof)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if proof == "" {
			if response.Code != http.StatusForbidden || f.calls.Load() != before {
				t.Fatal("cookie CSRF refusal bypassed", response.Code)
			}
		} else if response.Code != http.StatusOK || f.calls.Load() != before+1 {
			t.Fatal("original cookie proof not accepted", response.Code)
		}
	}
}

func TestIntegrationAdminActualRouterRevokesBeforeSecondRequest(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	f.probe.requests.Store(2)
	revoke := func() {
		if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: f.tokenID}, store.DeleteOptions{Manager: store.ManagerSystem})
		}); err != nil {
			t.Error("controlled revocation failed")
		}
	}
	f.remoteAction.Store(&revoke)
	before := f.calls.Load()
	w := integrationAdminCall(t, integrationAdminRouter(t, f), f, http.MethodPost, "/integrations/default/manual/check", api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion}, f.token)
	if w.Code == http.StatusOK || f.calls.Load() != before+1 {
		t.Fatal("revoked original caller reached another provider request", w.Code)
	}
}

func TestIntegrationAdminActualScopedTokenAndWriterRefusal(t *testing.T) {
	for _, mode := range []string{"scoped-token", "writer-stopped"} {
		t.Run(mode, func(t *testing.T) {
			f := newIntegrationOperationFixture(t)
			if mode == "scoped-token" {
				err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					var token internalv1.APIToken
					if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: f.tokenID}, &token); err != nil {
						return err
					}
					token.Spec.Scopes = []string{"viewer"}
					_, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: token.ResourceVersion})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				awaitServingHTTPView(t, f.s, func(view *servingauth.View) bool {
					p, err := view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: f.token}, time.Now())
					if err != nil {
						return false
					}
					d, err := view.Authorize(p, rbac.Attributes{Resource: "integrations", Verb: "check", Namespace: "default", Name: "manual", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}, time.Now())
					return err == nil && !d.Allowed
				})
			} else {
				f.stop()
			}
			before := f.calls.Load()
			w := integrationAdminCall(t, integrationAdminRouter(t, f), f, http.MethodPost, "/integrations/default/manual/check", api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion}, f.token)
			if w.Code == http.StatusOK || f.calls.Load() != before {
				t.Fatal("refused caller or writer reached provider", w.Code)
			}
			if mode == "scoped-token" && w.Code != http.StatusForbidden {
				t.Fatal("scoped token did not preserve explicit denial", w.Code)
			}
		})
	}
}
