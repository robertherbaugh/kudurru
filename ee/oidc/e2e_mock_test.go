package oidc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// TestMockOPEndToEnd runs the end-to-end suite against the in-process mock
// OP, so `go test ./...` covers the whole flow without dex, then exercises
// claim shapes and failure modes dex cannot be made to produce.
func TestMockOPEndToEnd(t *testing.T) {
	h := newHarness(t)
	const id = "mock"
	op := newMockOP(t, "kudurru", "mock-client-secret", h.redirectURL(id))
	idp := testIDP{id: id, issuer: op.issuer(), clientID: "kudurru", clientSecret: "mock-client-secret"} //nolint:gosec // G101: test-only secret

	user := func(login, sub, preferred string, groups any, extra map[string]any) {
		claims := map[string]any{
			"email": login, "email_verified": true, "name": preferred, "preferred_username": preferred,
		}
		if groups != nil {
			claims["groups"] = groups
		}
		for k, v := range extra {
			claims[k] = v
		}
		op.addUser(login, e2ePassword, sub, claims)
	}
	user(userAdmin, "sub-admin", "admin", []string{"kudurru-admins", "staff"}, nil)
	user(userNobody, "sub-nobody", "nobody", []string{"staff"}, nil)
	user(userMany, "sub-many", "many", manyUserGroups(), nil)
	user("string-group@example.com", "sub-sg", "sg", "kudurru-admins", nil)
	user("roles@example.com", "sub-roles", "roles", nil, map[string]any{"roles": []string{"kudurru-viewers"}})
	user("overage@example.com", "sub-overage", "overage", nil, map[string]any{
		"_claim_names":   map[string]string{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]string{"endpoint": "https://graph.example/getMemberObjects"}},
	})
	user("known@example.com", "sub-known", "known", []string{"staff"}, nil)
	user("unverified@example.com", "sub-unverified", "", []string{"kudurru-viewers"}, map[string]any{"email_verified": false, "preferred_username": nil})

	h.configure(idp, keyA, nil)
	runSuite(t, h, idp)

	// The rest are mock-only. Each resets the harness customisations.
	run := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			h.t = t
			h.clk.Reset()
			h.tweak, h.resolver = nil, nil
			op.setTokenHook(nil)
			defer func() { h.tweak, h.resolver = nil, nil; op.setTokenHook(nil) }()
			h.configure(idp, keyA, nil)
			fn(t)
		})
	}

	run("groups claim as a single string", func(t *testing.T) {
		b := h.newBrowser()
		b.mustLogin("string-group@example.com")
		if _, me := b.me(); strings.Join(me.Roles, ",") != "admin" {
			t.Errorf("roles = %v", me.Roles)
		}
	})

	run("roles claim is read as groups", func(t *testing.T) {
		b := h.newBrowser()
		b.mustLogin("roles@example.com")
		if _, me := b.me(); strings.Join(me.Roles, ",") != "viewer" {
			t.Errorf("roles = %v", me.Roles)
		}
	})

	run("username falls back to sub when email is unverified", func(t *testing.T) {
		b := h.newBrowser()
		b.mustLogin("unverified@example.com")
		if _, me := b.me(); me.Principal.Name != id+":sub-unverified" {
			t.Errorf("name = %q", me.Principal.Name)
		}
	})

	run("distributed groups claim is refused with an explanation", func(t *testing.T) {
		resp, body := h.newBrowser().login("overage@example.com", "/")
		wantLoginError(t, resp, body, http.StatusForbidden, CodeDistributedClaim)
	})

	run("several audiences without azp are refused", func(t *testing.T) {
		op.setTokenHook(func(_ string, c map[string]any) { c["aud"] = []string{"kudurru", "other-app"} })
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeAudience)
	})

	run("token for another client is refused", func(t *testing.T) {
		op.setTokenHook(func(_ string, c map[string]any) { c["aud"] = "other-app" })
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeInvalidIDToken)
	})

	run("foreign azp is refused", func(t *testing.T) {
		op.setTokenHook(func(_ string, c map[string]any) { c["azp"] = "other-app" })
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeAudience)
	})

	run("several audiences with our azp are accepted", func(*testing.T) {
		op.setTokenHook(func(_ string, c map[string]any) {
			c["aud"], c["azp"] = []string{"kudurru", "other-app"}, "kudurru"
		})
		h.newBrowser().mustLogin(userAdmin)
	})

	run("mismatched at_hash is refused", func(t *testing.T) {
		op.setTokenHook(func(_ string, c map[string]any) { c["at_hash"] = "AAAAAAAAAAAAAAAAAAAAAA" })
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeInvalidIDToken)
	})

	run("claim validation rule failure is refused", func(t *testing.T) {
		h.tweak = func(c *ProviderConfig) {
			c.ClaimValidationRules = []ClaimValidationRule{{Claim: "hd", RequiredValue: "example.com"}}
		}
		h.configure(idp, keyA, nil)
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusForbidden, CodeClaimRule)
		op.setTokenHook(func(_ string, c map[string]any) { c["hd"] = "example.com" })
		h.newBrowser().mustLogin(userAdmin)
	})

	run("IdP error parameter is reported", func(t *testing.T) {
		b := h.newBrowser()
		authURL := b.startLogin("/")
		resp, body := b.get(h.redirectURL(id) + "?error=access_denied&state=" + authURL.Query().Get("state"))
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeIDPError)
	})

	run("pre-auth cookie expires after 10 minutes", func(t *testing.T) {
		b := h.newBrowser()
		cb := b.authenticateAtIDP(b.startLogin("/"), userAdmin)
		h.clk.Advance(PreAuthLifetime + time.Second)
		resp, body := b.finish(cb)
		wantLoginError(t, resp, body, http.StatusBadRequest, CodeExpiredPreAuth)
	})

	run("defaultRole with autoProvision always admits unbound users", func(t *testing.T) {
		h.tweak = func(c *ProviderConfig) {
			c.Provisioning = Provisioning{AutoProvision: AutoProvisionAlways, DefaultRole: "viewer"}
		}
		h.configure(idp, keyA, nil)
		b := h.newBrowser()
		b.mustLogin(userNobody)
		if _, me := b.me(); strings.Join(me.Roles, ",") != "viewer" {
			t.Errorf("roles = %v", me.Roles)
		}
	})

	run("autoProvision never admits only known identities", func(t *testing.T) {
		h.tweak = func(c *ProviderConfig) {
			c.Provisioning = Provisioning{AutoProvision: AutoProvisionNever, DefaultRole: "viewer"}
		}
		h.configure(idp, keyA, nil)
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusForbidden, CodeNotProvisioned)
		b := h.newBrowser()
		b.mustLogin("known@example.com")
		if _, me := b.me(); strings.Join(me.Roles, ",") != "viewer" {
			t.Errorf("roles = %v", me.Roles)
		}
	})

	run("role snapshot over the 4 KiB guard is a clear error", func(t *testing.T) {
		h.resolver = RoleResolverFunc(func(context.Context, auth.Principal) ([]auth.Grant, error) {
			out := make([]auth.Grant, 120)
			for i := range out {
				out[i] = auth.Grant{Role: "zone-admin", ScopeKind: "zone", ScopeName: fmt.Sprintf("zone-%03d-example-com", i)}
			}
			return out, nil
		})
		h.configure(idp, keyA, nil)
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusInternalServerError, CodeSessionTooLarge)
	})

	run("resolver failure is an internal error, not a login", func(t *testing.T) {
		h.resolver = RoleResolverFunc(func(context.Context, auth.Principal) ([]auth.Grant, error) {
			return nil, fmt.Errorf("store unavailable")
		})
		h.configure(idp, keyA, nil)
		resp, body := h.newBrowser().login(userAdmin, "/")
		wantLoginError(t, resp, body, http.StatusInternalServerError, CodeInternal)
	})
}

// TestProviderIdPDown checks lazy discovery: kudurru starts while the IdP
// is unreachable and logins fail with idp_unavailable instead of hanging
// or crashing.
func TestProviderIdPDown(t *testing.T) {
	h := newHarness(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()
	h.configure(testIDP{id: "down", issuer: dead, clientID: "kudurru", clientSecret: "x"}, keyA, nil)
	resp, body := h.newBrowser().get(h.srv.URL + "/api/v1/auth/oidc/down/start")
	wantLoginError(t, resp, body, http.StatusServiceUnavailable, CodeIDPUnavailable)
	if resp, _ := h.newBrowser().get(h.srv.URL + "/api/v1/auth/oidc/unknown/start"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown provider: %d", resp.StatusCode)
	}
}
