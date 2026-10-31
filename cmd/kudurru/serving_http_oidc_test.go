package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type servingBrowserFixture struct {
	*servingHTTPFixture
	op         *servingBrowserOP
	stopWriter func()
}

func newServingBrowserFixture(t *testing.T) *servingBrowserFixture {
	t.Helper()
	return newServingBrowserFixtureConfigured(t, nil)
}
func newServingBrowserFixtureConfigured(t *testing.T, configure func(*servingBrowserOP)) *servingBrowserFixture {
	t.Helper()
	op := newServingBrowserOP(t)
	if configure != nil {
		configure(op)
	}
	base := newServingRuntimeFixture(t)
	if err := base.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts := base.opts
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(secretFile, []byte(op.ClientSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, op.CA, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = []map[string]any{{"id": "corp", "issuer": op.Issuer("corp"), "clientID": op.ClientID, "clientSecretFile": secretFile, "redirectURL": "https://kudurru.test/api/v1/auth/oidc/corp/callback",
		"claimValidationRules": []any{map[string]any{"expression": "claims.groups.exists(g, g == 'dns')"}}}}
	cfg.Auth.ProviderResources = config.ProviderResources{AllowedOrigins: []string{op.Server.URL}, LocalOrigins: []string{op.Server.URL}, Algorithms: []string{"EdDSA"}, SystemTrust: false, CAFiles: []string{caFile}}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	opts.License, err = license.New(license.Options{Keyring: license.Keyring{"browser": pub}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	signed, err := license.Sign(key, "browser", license.Claims{ID: "browser-test", Issuer: license.Issuer, Licensee: "browser test", Plan: license.PlanEnterprise, Nodes: 3, Features: []license.Feature{license.FeatureOIDC, license.FeatureRBAC}, IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.License.Set(signed, "test"); err != nil {
		t.Fatal(err)
	}
	opts.Config = cfg
	opts.Now = time.Now
	if err := opts.Store.Command(t.Context(), opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "browser-view", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "corp:alice"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	runServingFixture(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("browser-http-test").Run(ctx, func(ctx context.Context) {
			if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error("browser writer ended", err)
			}
		}, nil)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("browser writer did not join")
			}
		})
	}
	t.Cleanup(stop)
	var providerCreatedAt time.Time
	awaitServingHTTPView(t, s, func(v *servingauth.View) bool {
		info, err := v.ProviderInfo("corp")
		if err == nil {
			providerCreatedAt = info.Descriptor.CreatedAt
		}
		return err == nil && info.Availability == servingauth.ProviderReady && s.primary.driver.Status().Ready
	})
	// Real OP numeric dates must be ACTUALLY later than the provider's
	// nanosecond creation time, including first activation with no floor row.
	// externalFloors rejects an iat before CreatedAt even without invalidation.
	awaitServingRuntime(t, s, func() bool { return time.Now().Unix() > providerCreatedAt.Unix() })
	// Also wait past any nanosecond floor raised at authority replacement;
	// never fabricate an iat, retry a refused callback, or relax production gates.
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	for _, floor := range state.ProviderFloors {
		awaitServingRuntime(t, s, func() bool { return time.Now().Unix() > floor.ValidAfter.Unix() })
	}
	h, err := newServingHTTP(servingHTTPOptions{Runtime: s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.ProviderHandler = h.CapturedBrowserHandler()
	server, err := api.New(api.Options{Store: s.opts.Store, Validator: s.opts.Validator, Config: s.opts.Config, CanonicalSettings: s, Authenticate: h.Middleware, AuthHandler: h.Handler(), Authorizer: h.Authorizer(), Roles: h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return &servingBrowserFixture{servingHTTPFixture: &servingHTTPFixture{servingCommandFixture: &servingCommandFixture{s: s}, h: h, server: server}, op: op, stopWriter: stop}
}

func (f *servingBrowserFixture) begin(t *testing.T, next string) (*http.Cookie, string) {
	t.Helper()
	w := f.call(t, "GET", "/auth/oidc/corp/start?next="+url.QueryEscape(next), "", "", nil, "")
	expectServingHTTPStatus(t, w, 302)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "kudurru_session_oidc" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].MaxAge <= 0 || cookies[0].MaxAge > 600 || w.Body.Len() != 0 {
		t.Fatal("private pending cookie/redirect contract")
	}
	return cookies[0], f.op.Complete(t, w.Header().Get("Location"))
}
func (f *servingBrowserFixture) callback(t *testing.T, callback string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(callback)
	if err != nil {
		t.Fatal("invalid callback")
	}
	return f.call(t, "GET", strings.TrimPrefix(u.RequestURI(), "/api/v1"), "", "", cookie, "")
}
func (f *servingBrowserFixture) security(t *testing.T) servingauth.Security {
	t.Helper()
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestServingHTTPBrowserActualTLSJWTAndDurableSession(t *testing.T) {
	f := newServingBrowserFixture(t)
	status := f.call(t, "GET", "/auth/status", "", "", nil, "")
	expectServingHTTPStatus(t, status, 200)
	var out api.AuthStatus
	if err := json.Unmarshal(status.Body.Bytes(), &out); err != nil || len(out.Providers) != 1 || out.Providers[0].ID != "corp" {
		t.Fatal("captured provider not advertised", err)
	}
	cookie, callback := f.begin(t, "/zones?tab=records")
	if len(f.security(t).BrowserAttempts) != 1 || len(f.security(t).ExternalSessions) != 0 {
		t.Fatal("start enrolled prematurely")
	}
	w := f.callback(t, callback, cookie)
	expectServingHTTPStatus(t, w, 302)
	if w.Header().Get("Location") != "/zones?tab=records" || w.Body.Len() != 0 || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("unsafe callback response")
	}
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "kudurru_session" && c.MaxAge > 0 {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing enrolled session cookie")
	}
	state := f.security(t)
	if len(state.BrowserAttempts) != 0 || len(state.ExternalSessions) != 1 || len(state.ExternalIdentities) != 1 {
		t.Fatal("pending consume/session enrollment not atomic")
	}
	me := f.call(t, "GET", "/auth/me", "", "", sessionCookie, "")
	expectServingHTTPStatus(t, me, 200)
	var info api.Me
	if err := json.Unmarshal(me.Body.Bytes(), &info); err != nil || info.CSRF == "" || info.Principal.Name != "corp:alice" || info.Principal.Auth != "cookie" {
		t.Fatal("enrolled external session mismatch", err)
	}
	before := f.op.requests.Load()
	expectServingHTTPStatus(t, f.callback(t, callback, cookie), 401)
	if f.op.requests.Load() != before {
		t.Fatal("consumed cookie performed another exchange")
	}
	for _, private := range []string{"NEVER-RETAIN-REFRESH", "browser-test-access-token", "evidence", "csrfHash", f.op.ClientSecret} {
		if strings.Contains(me.Body.String(), private) {
			t.Fatal("private OIDC material reached HTTP")
		}
	}
}

func TestServingHTTPBrowserInvalidProofRollsBackPendingConsumption(t *testing.T) {
	for _, mode := range []string{"nonce", "at_hash", "expired", "upstream-error", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			cookie, callback := f.begin(t, "/")
			f.op.mu.Lock()
			f.op.mode = mode
			f.op.mu.Unlock()
			before := f.security(t)
			w := f.callback(t, callback, cookie)
			if mode == "upstream-error" || mode == "oversize" {
				expectServingHTTPStatus(t, w, 503)
			} else {
				expectServingHTTPStatus(t, w, 401)
			}
			after := f.security(t)
			if len(after.BrowserAttempts) != 1 || after.BrowserAttempts[0] != before.BrowserAttempts[0] || len(after.ExternalSessions) != 0 || len(after.ExternalIdentities) != 0 || after.Epoch != before.Epoch {
				t.Fatal("failed login consumed pending or partially synced identity")
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == "kudurru_session" && c.MaxAge >= 0 {
					t.Fatal("failure sealed a session")
				}
			}
			if strings.Contains(w.Body.String(), "NEVER-EXPOSE-UPSTREAM") || strings.Contains(w.Body.String(), callback) {
				t.Fatal("upstream/query diagnostic escaped")
			}
		})
	}
}

func TestServingHTTPBrowserStrictStateNextCookieAndQuery(t *testing.T) {
	f := newServingBrowserFixture(t)
	cookie, callback := f.begin(t, "/")
	u, _ := url.Parse(callback)
	for _, mode := range []string{"state", "duplicate state", "duplicate code", "wrong route", "missing cookie", "duplicate cookie", "issuer", "provider error"} {
		t.Run(mode, func(t *testing.T) {
			copyURL := *u
			q := copyURL.Query()
			usedCookie := cookie
			switch mode {
			case "state":
				q.Set("state", "wrong")
			case "duplicate state":
				q.Add("state", q.Get("state"))
			case "duplicate code":
				q.Add("code", q.Get("code"))
			case "wrong route":
				copyURL.Path = "/api/v1/auth/oidc/other/callback"
			case "missing cookie":
				usedCookie = nil
			case "issuer":
				q.Set("iss", "https://other.example")
			case "provider error":
				q.Set("error", "access_denied")
				q.Set("error_description", "PRIVATE-UPSTREAM")
			}
			copyURL.RawQuery = q.Encode()
			before := f.op.requests.Load()
			var w *httptest.ResponseRecorder
			if mode == "duplicate cookie" {
				r := httptest.NewRequest("GET", copyURL.String(), nil)
				r.AddCookie(cookie)
				r.AddCookie(cookie)
				r.RemoteAddr = "192.0.2.20:3131"
				w = httptest.NewRecorder()
				f.server.ServeHTTP(w, r)
			} else {
				w = f.callback(t, copyURL.String(), usedCookie)
			}
			want := 401
			if strings.HasPrefix(mode, "duplicate ") && mode != "duplicate cookie" {
				want = 400
			}
			expectServingHTTPStatus(t, w, want)
			if f.op.requests.Load() != before || strings.Contains(w.Body.String(), "PRIVATE-UPSTREAM") {
				t.Fatal("invalid preauth used network/leaked diagnostics")
			}
			if len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
				t.Fatal("preauth not cleared")
			}
		})
	}
	// Bad next never creates pending authority or an open redirect.
	before := f.security(t).Epoch
	w := f.call(t, "GET", "/auth/oidc/corp/start?next="+url.QueryEscape("//attacker.example"), "", "", nil, "")
	expectServingHTTPStatus(t, w, 400)
	if f.security(t).Epoch != before {
		t.Fatal("unsafe next staged state")
	}
}

func TestServingHTTPBrowserConcurrentCallbacksAndHostReplacementDuringIO(t *testing.T) {
	for _, mode := range []string{"concurrent", "host"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			cookie, callback := f.begin(t, "/")
			f.op.mu.Lock()
			f.op.entered = make(chan struct{}, 1)
			f.op.resume = make(chan struct{})
			entered, resume := f.op.entered, f.op.resume
			f.op.mu.Unlock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.callback(t, callback, cookie) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("token exchange not entered")
			}
			if mode == "concurrent" {
				expectServingHTTPStatus(t, f.callback(t, callback, cookie), 429)
			} else {
				_ = f.s.opts.License.Set("invalid-replacement", "test")
			}
			close(resume)
			w := <-done
			if mode == "concurrent" {
				expectServingHTTPStatus(t, w, 302)
			} else {
				expectServingHTTPStatus(t, w, 503)
			}
			if f.op.requests.Load() != 1 {
				t.Fatal("one-shot exchange retried")
			}
			if mode == "host" && len(f.security(t).ExternalSessions) != 0 {
				t.Fatal("changed host issued enrollment")
			}
		})
	}
}

func TestServingHTTPBrowserAnonymousStartAdmissionAndOriginalWriter(t *testing.T) {
	f := newServingBrowserFixture(t)
	for range 5 {
		f.begin(t, "/")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/oidc/corp/start", "", "", nil, ""), 429)
	if len(f.security(t).BrowserAttempts) != 5 {
		t.Fatal("start limiter did not bound durable rows")
	}
	f.stopWriter()
	r := httptest.NewRequest("GET", "https://kudurru.test/api/v1/auth/oidc/corp/start", nil)
	r.RemoteAddr = "192.0.2.99:1"
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code >= 200 && w.Code < 400 {
		t.Fatal("stopped writer admitted start")
	}
	if len(f.security(t).BrowserAttempts) != 5 {
		t.Fatal("stopped writer staged pending state")
	}
}
