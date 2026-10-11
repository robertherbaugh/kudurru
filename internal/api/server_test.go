package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type staticMembership struct{ peers []cluster.Peer }

func (m staticMembership) Leader(context.Context) (cluster.Peer, error) { return m.peers[0], nil }
func (m staticMembership) Peers(context.Context) ([]cluster.Peer, error) {
	return m.peers, nil
}

func TestNodesAndCluster(t *testing.T) {
	checker := health.New(nil)
	checker.Set("dns", false, "binding")
	h := newHarness(t, func(o *Options) {
		o.Health = checker
		o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-a", Self: true}, {Name: "node-b", Address: "10.0.0.2:8082"}}}
	})
	var nodes struct {
		Items []NodeInfo `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/nodes", "admin", nil).want(http.StatusOK).json(&nodes)
	if len(nodes.Items) != 2 || !nodes.Items[0].Self || !nodes.Items[0].Leader || nodes.Items[0].Ready == nil || *nodes.Items[0].Ready ||
		nodes.Items[0].Version != "v0.1.0-test" || nodes.Items[1].Ready != nil {
		t.Fatalf("nodes %+v", nodes.Items)
	}
	var nh NodeHealth
	h.do(http.MethodGet, "/api/v1/nodes/node-a/health", "admin", nil).want(http.StatusOK).json(&nh)
	if nh.Ready || nh.Health["components"] == nil {
		t.Fatalf("health %+v", nh)
	}
	h.do(http.MethodGet, "/api/v1/nodes/node-b/health", "admin", nil).want(http.StatusNotImplemented)
	h.do(http.MethodGet, "/api/v1/nodes/node-z/health", "admin", nil).want(http.StatusNotFound)
	var ci ClusterInfo
	h.do(http.MethodGet, "/api/v1/cluster", "admin", nil).want(http.StatusOK).json(&ci)
	if ci.Nodes != 2 || ci.Leader != "node-a" || ci.Driver != "memory" && ci.Driver != "" {
		t.Fatalf("cluster %+v", ci)
	}
	h.do(http.MethodGet, "/api/v1/edge", "admin", nil).want(http.StatusOK)
	var msgs struct {
		Items []DiagnosisMessage `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&msgs)
	if len(msgs.Items) != 1 || msgs.Items[0].Source != "health" || msgs.Items[0].Node != "node-a" {
		t.Fatalf("messages %+v", msgs.Items)
	}
}

func TestInfoMessagesFromObjects(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("apiVersion: kudurru.io/v1alpha1\nkind: DNSZone\nmetadata: {name: x}\nspec: {zoneName: x., type: nope}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	l := loaderFor(t, h.st, dir)
	l.Load(context.Background())
	h2 := newHarness(t, func(o *Options) { o.Store = h.st; o.Objects = l })
	var msgs struct {
		Items []DiagnosisMessage `json:"items"`
	}
	h2.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&msgs)
	if len(msgs.Items) != 1 || msgs.Items[0].Source != "objects" || msgs.Items[0].Severity != "error" || !strings.Contains(msgs.Items[0].Message, "bad.yaml") {
		t.Fatalf("messages %+v", msgs.Items)
	}
}

func TestAuditWithSQLite(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: testValidator(t), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := audit.Multi{audit.SQLite{Store: st}, audit.Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	h := newHarness(t, func(o *Options) { o.Store = st; o.Audit = rec })
	// A store write: audited by the driver with the API principal as actor.
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusCreated)
	// A denied attempt: audited by the API.
	h.do(http.MethodDelete, "/api/v1/objects/dnszones/example", "viewer", nil).want(http.StatusForbidden)

	var page AuditPage
	h.do(http.MethodGet, "/api/v1/audit", "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 2 {
		t.Fatalf("audit %+v", page.Items)
	}
	w := page.Items[0]
	if w.Resource.Kind != "DNSZone" || w.Resource.Name != "example" || w.Actor.Name != "admin" || w.Result != "success" || w.Hash == "" {
		t.Fatalf("store write row %+v", w)
	}
	d := page.Items[1]
	if d.Result != "denied" || d.Actor.Name != "viewer" || d.Action != "zones.delete" || d.PrevHash != w.Hash {
		t.Fatalf("denied row %+v", d)
	}
	h.do(http.MethodGet, "/api/v1/audit?actor=viewer", "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 {
		t.Fatalf("actor filter %+v", page.Items)
	}
	h.do(http.MethodGet, "/api/v1/audit?limit=1", "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.CursorNext == "" {
		t.Fatalf("page %+v", page)
	}
	h.do(http.MethodGet, "/api/v1/audit?cursor="+page.CursorNext, "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.Items[0].Result != "denied" {
		t.Fatalf("page 2 %+v", page)
	}
	// Newest first, paged with continue.
	h.do(http.MethodGet, "/api/v1/audit?order=desc&limit=1", "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.Items[0].Result != "denied" || page.Continue == "" {
		t.Fatalf("desc page %+v", page)
	}
	h.do(http.MethodGet, "/api/v1/audit?order=desc&limit=1&continue="+page.Continue, "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.Items[0].ID != w.ID {
		t.Fatalf("desc page 2 %+v", page)
	}
	var last AuditPage
	h.do(http.MethodGet, "/api/v1/audit?order=desc&limit=1&continue="+page.Continue, "auditor", nil).want(http.StatusOK).json(&last)
	if len(last.Items) != 0 || last.Continue != "" {
		t.Fatalf("desc page 3 %+v", last)
	}
	h.do(http.MethodGet, "/api/v1/audit?order=sideways", "auditor", nil).want(http.StatusBadRequest)
	h.do(http.MethodGet, "/api/v1/audit/export", "auditor", nil).want(http.StatusForbidden) // audit.export is admin-only
	r := h.do(http.MethodGet, "/api/v1/audit/export", "admin", nil).want(http.StatusOK)
	lines := strings.Split(strings.TrimSpace(r.Body.String()), "\n")
	if len(lines) < 3 || r.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("export %q", r.Body.String())
	}
	var ev audit.Event
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil || ev.Result != "denied" {
		t.Fatalf("export line %v %+v", err, ev)
	}
	if err := st.VerifyAudit(context.Background()); err != nil {
		t.Fatalf("hash chain: %v", err)
	}
	h.do(http.MethodGet, "/api/v1/audit", "operator", nil).want(http.StatusForbidden)
}

func TestAuditNotStored(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Audit = audit.Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))} })
	h.do(http.MethodGet, "/api/v1/audit", "admin", nil).want(http.StatusServiceUnavailable)
}

func TestUIMount(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":     {Data: []byte(`<!doctype html><html><head><base href="/"><title>app</title></head><body></body></html>`)},
		"assets/app.js":  {Data: []byte("console.log(1)")},
		"favicon.svg":    {Data: []byte("<svg/>")},
		"assets/app.css": {Data: []byte("body{}")},
	}
	h := newHarness(t, func(o *Options) { o.UI = ui })
	r := h.do(http.MethodGet, "/", "", nil).want(http.StatusOK)
	if !strings.Contains(r.Body.String(), "<title>app</title>") {
		t.Fatal("index not served")
	}
	r = h.do(http.MethodGet, "/assets/app.js", "", nil).want(http.StatusOK)
	if r.Body.String() != "console.log(1)" || !strings.Contains(r.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset %q %q", r.Body.String(), r.Header().Get("Cache-Control"))
	}
	// Client-side routes fall back to index.html; missing files do not.
	if r = h.do(http.MethodGet, "/zones/example", "", nil).want(http.StatusOK); !strings.Contains(r.Body.String(), "<title>app</title>") {
		t.Fatal("SPA fallback")
	}
	h.do(http.MethodGet, "/assets/missing.js", "", nil).want(http.StatusNotFound)
	// API paths never get the SPA.
	h.do(http.MethodGet, "/api/v1/missing", "admin", nil).want(http.StatusNotFound)
	h.do(http.MethodPost, "/zones", "", nil).want(http.StatusMethodNotAllowed)

	// Without a bundle: the placeholder.
	h = newHarness(t)
	if r = h.do(http.MethodGet, "/", "", nil).want(http.StatusOK); !strings.Contains(r.Body.String(), "dashboard is not part of this build") {
		t.Fatal("placeholder")
	}
}

func TestPathPrefix(t *testing.T) {
	ui := fstest.MapFS{"index.html": {Data: []byte(`<html><head><base href="/"></head></html>`)}}
	h := newHarness(t, func(o *Options) { o.Config.Listeners.API.PathPrefix = "/kudurru"; o.UI = ui })
	h.do(http.MethodGet, "/kudurru/api/v1/version", "", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/version", "", nil).want(http.StatusNotFound)
	if r := h.do(http.MethodGet, "/kudurru", "", nil); r.Code != http.StatusMovedPermanently || r.Header().Get("Location") != "/kudurru/" {
		t.Fatalf("redirect %d %q", r.Code, r.Header().Get("Location"))
	}
	r := h.do(http.MethodGet, "/kudurru/queries", "", nil).want(http.StatusOK)
	if !strings.Contains(r.Body.String(), `<base href="/kudurru/">`) {
		t.Fatalf("base href not rewritten: %s", r.Body.String())
	}
	r = h.do(http.MethodPut, "/kudurru/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusCreated)
	if r.Header().Get("Location") != "/kudurru/api/v1/objects/dnszones/example" {
		t.Fatalf("Location %q", r.Header().Get("Location"))
	}
}

func TestAuthForwarding(t *testing.T) {
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/status":
			_, _ = io.WriteString(w, `{"localLogin":true,"localLoginMode":"enabled","providers":[],"hasDefaultCredentials":true}`)
		case "/api/v1/auth/login":
			body, _ := io.ReadAll(r.Body)
			http.SetCookie(w, &http.Cookie{Name: "kudurru_session", Value: "x", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			_, _ = w.Write(body)
		case "/api/v1/auth/oidc/corp/start":
			http.Redirect(w, r, "https://idp.example/authorize", http.StatusFound)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	limited := 0
	h := newHarness(t, func(o *Options) {
		o.AuthHandler = authHandler
		o.RateLimit = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				limited++
				if r.Header.Get("X-Test-Limit") != "" {
					w.Header().Set("Retry-After", "60")
					writeError(w, r, newError(http.StatusTooManyRequests, KeyRateLimited, "too many attempts", ""))
					return
				}
				next.ServeHTTP(w, r)
			})
		}
	})
	var st AuthStatus
	h.do(http.MethodGet, "/api/v1/auth/status", "", nil).want(http.StatusOK).json(&st)
	if !st.LocalLogin || st.LocalLoginMode != "enabled" || !st.HasDefaultCredentials {
		t.Fatalf("status %+v", st)
	}
	r := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "admin", "password": "pw"}).want(http.StatusOK)
	if !strings.Contains(r.Body.String(), `"username":"admin"`) || r.Header().Get("Set-Cookie") == "" {
		t.Fatalf("login body not forwarded: %s", r.Body.String())
	}
	if r := h.do(http.MethodGet, "/api/v1/auth/oidc/corp/start", "", nil); r.Code != http.StatusFound {
		t.Fatalf("oidc start %d", r.Code)
	}
	if r := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{}, "X-Test-Limit", "1"); r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate limit %d", r.Code)
	}
	if limited != 2 {
		t.Fatalf("rate limiter saw %d requests, want 2 (login only)", limited)
	}
	h.do(http.MethodPost, "/api/v1/auth/logout", "", nil).want(http.StatusUnauthorized)
	// Without an auth handler the routes answer 501.
	newHarness(t).do(http.MethodGet, "/api/v1/auth/status", "", nil).want(http.StatusNotImplemented)
}

// TestForwardedAuthBodyCap is the regression test of the M5 hardening
// "anonymous login requests are buffered up to 32 MiB": the forwarded auth
// routes inherited the 32 MiB object cap and huma read the whole body into
// memory before the auth handler (which caps at 64 KiB) saw it, so
// concurrent anonymous logins could exhaust memory.
func TestForwardedAuthBodyCap(t *testing.T) {
	called := 0
	h := newHarness(t, func(o *Options) {
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(http.StatusOK) })
	})
	big := `{"username":"admin","password":"` + strings.Repeat("x", 1<<20) + `"}`
	if r := h.do(http.MethodPost, "/api/v1/auth/login", "", big, "Content-Type", "application/json"); r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("1 MiB login body: %d", r.Code)
	}
	if called != 0 {
		t.Fatal("the oversized body reached the auth handler")
	}
	h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "admin", "password": "pw"}).want(http.StatusOK)
	if called != 1 {
		t.Fatalf("auth handler calls %d, want 1", called)
	}
}

func TestMeWithBearerRoles(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Roles = func(_ context.Context, p auth.Principal) []string { return []string{"zone-admin@zone:" + p.Name} }
	})
	var me Me
	h.do(http.MethodGet, "/api/v1/auth/me", "zoneadmin", nil).want(http.StatusOK).json(&me)
	if len(me.Roles) != 1 || me.Roles[0] != "zone-admin@zone:zoneadmin" || me.CSRF != "" || len(me.Permissions) != 0 || me.Principal.Auth != "token" {
		t.Fatalf("me %+v", me)
	}
	h.do(http.MethodGet, "/api/v1/auth/me", "admin", nil).want(http.StatusOK).json(&me)
	if len(me.Permissions) != 22 {
		t.Fatalf("admin permissions %d: %v", len(me.Permissions), me.Permissions)
	}
}

// A local user's cookie session has no roles snapshot: /auth/me answers
// the live grants of Options.Roles, as the login answer does.
func TestMeCookieSessionLiveRoles(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Roles = func(context.Context, auth.Principal) []string { return []string{"admin"} }
	})
	r := h.req(http.MethodGet, "/api/v1/auth/me", "", nil)
	r.AddCookie(&http.Cookie{Name: apitest.CookieName, Value: "admin", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	var me Me
	h.serve(r).want(http.StatusOK).json(&me)
	if len(me.Roles) != 1 || me.Roles[0] != "admin" || me.CSRF != apitest.CSRFToken || me.MustChangePassword {
		t.Fatalf("me %+v", me)
	}
}

func TestMeMustChangePassword(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Authenticate = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := auth.Principal{Kind: auth.KindUser, ID: "admin", Name: "admin", AuthMethod: "cookie", MustChangePassword: true}
				next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
			})
		}
	})
	var me Me
	h.do(http.MethodGet, "/api/v1/auth/me", "", nil).want(http.StatusOK).json(&me)
	if !me.MustChangePassword || len(me.Permissions) != 0 {
		t.Fatalf("me %+v", me)
	}
	if r := h.do(http.MethodGet, "/api/v1/zones", "", nil).want(http.StatusForbidden); r.errKey() != "password_change_required" {
		t.Fatalf("zones: %s", r.Body.String())
	}
}

func TestMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	h := newHarness(t, func(o *Options) { o.Registerer = reg })
	h.do(http.MethodGet, "/api/v1/zones", "admin", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/zones/x", "admin", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/zones/y", "admin", nil).want(http.StatusNotFound)
	got := testutil.ToFloat64(h.srv.metrics.requests.WithLabelValues("GET", "GET /api/v1/zones/{zone}", "404"))
	if got != 2 {
		t.Fatalf("requests{route=/zones/{zone},code=404} = %v", got)
	}
	if n, err := testutil.GatherAndCount(reg, "kudurru_api_request_duration_seconds"); err != nil || n != 2 {
		t.Fatalf("duration series %d %v", n, err)
	}
}

func TestServeTLSSelfSigned(t *testing.T) {
	cfg := testConfig(t)
	cfg.Listeners.API.TLS.Auto = "selfsigned"
	h := newHarness(t, func(o *Options) { o.Config = cfg })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.srv.Serve(ctx, ln) }()

	var certPEM []byte
	for range 100 {
		if certPEM, err = os.ReadFile(filepath.Join(cfg.Node.DataDir, "keys", SelfSignedCertFile)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	var resp *http.Response
	for range 50 {
		resp, err = client.Get("https://" + ln.Addr().String() + "/api/v1/version")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("status %d HSTS %q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The pair is generated once and reused.
	before, _ := os.ReadFile(filepath.Join(cfg.Node.DataDir, "keys", SelfSignedCertFile))
	if _, _, err := EnsureSelfSigned(filepath.Join(cfg.Node.DataDir, "keys"), "node-a"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(cfg.Node.DataDir, "keys", SelfSignedCertFile))
	if string(before) != string(after) {
		t.Fatal("self-signed certificate regenerated")
	}
}

func TestRequestIDAndRealIP(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.Listeners.API.TrustedProxies = []string{"192.0.2.0/24"} })
	r := h.do(http.MethodDelete, "/api/v1/objects/dnszones/x", "viewer", nil, "X-Forwarded-For", "203.0.113.7, 192.0.2.99", "X-Request-Id", "req-123")
	if r.Header().Get("X-Request-Id") != "req-123" {
		t.Fatalf("request id %q", r.Header().Get("X-Request-Id"))
	}
	evs := h.audit.Events()
	if len(evs) != 1 || evs[0].Request.IP != "203.0.113.7" || evs[0].Request.ID != "req-123" {
		t.Fatalf("audit request %+v", evs)
	}
	// Untrusted peers cannot choose their address or request id.
	h2 := newHarness(t)
	r = h2.do(http.MethodDelete, "/api/v1/objects/dnszones/x", "viewer", nil, "X-Forwarded-For", "203.0.113.7", "X-Request-Id", "req-123")
	if r.Header().Get("X-Request-Id") == "req-123" || h2.audit.Events()[0].Request.IP != "192.0.2.10" {
		t.Fatalf("untrusted headers believed: %+v", h2.audit.Events()[0].Request)
	}
}
