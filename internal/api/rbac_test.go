package api

import (
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
)

// TestRBACMatrix checks representative routes against the §8.5 matrix
// for every built-in role.
func TestRBACMatrix(t *testing.T) {
	h := newHarness(t)
	h.seed("api", zoneObj("example", "example.com."), zoneObj("other", "other.org."))
	type call struct {
		method, path string
		body         any
	}
	settingsPatch := map[string]any{"cache": map[string]any{"maxEntries": 1000}}
	cases := []struct {
		name string
		call call
		// allowed lists the users that pass authorization.
		allowed map[string]bool
	}{
		{"zones read", call{"GET", "/api/v1/zones/example", nil}, set("viewer", "auditor", "operator", "admin", "zoneadmin")},
		{"zones write", call{"PUT", "/api/v1/objects/dnszones/example", zoneDoc("example", "example.com.")}, set("admin", "zoneadmin")},
		{"zones create", call{"PUT", "/api/v1/objects/dnszones/brandnew", zoneDoc("brandnew", "brandnew.example.")}, set("admin")},
		{"zones delete", call{"DELETE", "/api/v1/zones/example?dryRun=true", nil}, set("admin", "zoneadmin")},
		{"other zone delete", call{"DELETE", "/api/v1/zones/other?dryRun=true", nil}, set("admin")},
		{"other zone write", call{"PUT", "/api/v1/objects/dnszones/other", zoneDoc("other", "other.org.")}, set("admin")},
		{"records write", call{"POST", "/api/v1/zones/example/records", ep("www", "A", "192.0.2.1")}, set("operator", "admin", "zoneadmin")},
		{"other records write", call{"POST", "/api/v1/zones/other/records", ep("www", "A", "192.0.2.1")}, set("operator", "admin")},
		{"blocklists read", call{"GET", "/api/v1/blocklists", nil}, set("viewer", "auditor", "operator", "admin")},
		{"blocklists write", call{"POST", "/api/v1/domains/deny/exact", map[string]any{"domains": []string{"ads.example"}}}, set("operator", "admin")},
		{"blocking read", call{"GET", "/api/v1/dns/blocking", nil}, set("viewer", "auditor", "operator", "admin")},
		{"blocking toggle", call{"POST", "/api/v1/dns/blocking", map[string]any{"blocking": true}}, set("operator", "admin")},
		{"settings read", call{"GET", "/api/v1/settings", nil}, set("viewer", "auditor", "operator", "admin")},
		{"settings write", call{"PATCH", "/api/v1/settings", settingsPatch}, set("admin")},
		{"node config", call{"PATCH", "/api/v1/node/config", map[string]any{"log": map[string]any{"level": "debug"}}}, set("admin")},
		{"querylog read", call{"GET", "/api/v1/queries", nil}, set("auditor", "operator", "admin")},
		{"querylog purge", call{"DELETE", "/api/v1/querylog", nil}, set("admin")},
		{"audit read", call{"GET", "/api/v1/audit", nil}, set("auditor", "admin")},
		{"cluster read", call{"GET", "/api/v1/nodes", nil}, set("admin")},
		{"iam read", call{"GET", "/api/v1/iam/rolebindings", nil}, set("admin")},
		{"own tokens", call{"GET", "/api/v1/auth/tokens", nil}, set("viewer", "auditor", "operator", "admin", "zoneadmin", "nobody")},
		{"me", call{"GET", "/api/v1/auth/me", nil}, set("viewer", "auditor", "operator", "admin", "zoneadmin", "nobody")},
	}
	users := []string{"viewer", "auditor", "operator", "admin", "zoneadmin", "nobody"}
	for _, tc := range cases {
		for _, u := range users {
			r := h.do(tc.call.method, tc.call.path, u, tc.call.body)
			denied := r.Code == http.StatusForbidden && r.errKey() == KeyForbidden
			if tc.allowed[u] == denied {
				t.Errorf("%s as %s: status %d (%s), allowed=%v", tc.name, u, r.Code, r.errKey(), tc.allowed[u])
			}
		}
	}
}

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func TestForbiddenEnvelope(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodPatch, "/api/v1/settings", "operator", map[string]any{"cache": map[string]any{"maxEntries": 10}}).want(http.StatusForbidden)
	m := r.m()["error"].(map[string]any)
	hint := m["hint"].(map[string]any)["missing"].(map[string]any)
	if hint["resource"] != "settings" || hint["verb"] != "write" || hint["scope"] != "cluster" {
		t.Fatalf("missing %v", hint)
	}
	evs := h.audit.Events()
	if len(evs) != 1 || evs[0].Result != "denied" || evs[0].Action != "settings.write" || evs[0].Request.Method != "PATCH" || evs[0].Authz != "binding" {
		t.Fatalf("denied audit %+v", evs)
	}
}

func TestCookieSessionNeedsCSRF(t *testing.T) {
	h := newHarness(t)
	cookie := &http.Cookie{Name: apitest.CookieName, Value: "operator", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	r := h.req(http.MethodPost, "/api/v1/dns/blocking", "", map[string]any{"blocking": true})
	r.AddCookie(cookie)
	if got := h.serve(r); got.Code != http.StatusForbidden || got.errKey() != KeyCSRF {
		t.Fatalf("no CSRF header: %d %s", got.Code, got.Body.String())
	}
	r = h.req(http.MethodPost, "/api/v1/dns/blocking", "", map[string]any{"blocking": true}, apitest.HeaderCSRF, apitest.CSRFToken)
	r.AddCookie(cookie)
	h.serve(r).want(http.StatusNotImplemented) // no blocking stage wired: past authorization
	// Safe methods need no CSRF header; /auth/me returns the token.
	r = h.req(http.MethodGet, "/api/v1/auth/me", "", nil)
	r.AddCookie(cookie)
	var me Me
	h.serve(r).want(http.StatusOK).json(&me)
	if me.CSRF != apitest.CSRFToken || me.Principal.Name != "operator" || me.Principal.Auth != "cookie" || len(me.Roles) != 1 {
		t.Fatalf("me %+v", me)
	}
	if verbs := me.Permissions["blocking"]; len(verbs) != 2 {
		t.Fatalf("blocking verbs %v", verbs)
	}
	if _, ok := me.Permissions["settings"]; !ok || len(me.Permissions["settings"]) != 1 {
		t.Fatalf("settings verbs %v", me.Permissions["settings"])
	}
}

func TestCrossOriginRejected(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodPost, "/api/v1/dns/blocking", "admin", map[string]any{"blocking": true},
		"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example")
	if r.Code != http.StatusForbidden || r.errKey() != KeyCSRF {
		t.Fatalf("cross-site POST: %d %s", r.Code, r.Body.String())
	}
	// Same-origin browser requests pass.
	h.do(http.MethodPost, "/api/v1/dns/blocking", "admin", map[string]any{"blocking": true}, "Sec-Fetch-Site", "same-origin").want(http.StatusNotImplemented)
}

func TestNoAuthorizerDeniesAll(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Authorizer = nil })
	h.do(http.MethodGet, "/api/v1/zones", "admin", nil).want(http.StatusOK) // scopable list: filtered to nothing
	h.do(http.MethodGet, "/api/v1/settings", "admin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/version", "", nil).want(http.StatusOK)
}
