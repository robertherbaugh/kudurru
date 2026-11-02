package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func authReloadHTTP(t *testing.T, f *servingHTTPFixture) {
	t.Helper()
	o := f.h.opts
	o.Cookie = session.CookieConfig{HostPrefix: true, TrustForwardedProto: true}
	h, err := newServingHTTP(o)
	if err != nil {
		t.Fatal("auth reload HTTP construction failed")
	}
	if o.ProviderHandler != nil {
		h.opts.ProviderHandler = h.CapturedBrowserHandler()
	}
	server, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, CanonicalSettings: f.s, Authenticate: h.Middleware, AuthHandler: h.Handler(), Authorizer: h.Authorizer(), Roles: h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal("auth reload router construction failed")
	}
	f.h, f.server = h, server
}

func authReloadPatch(t *testing.T, f *servingHTTPFixture, body string, match func(config.AuthControls) bool) {
	t.Helper()
	w := f.call(t, "PATCH", "/settings", body, f.token, nil, "")
	expectServingHTTPStatus(t, w, 200)
	var out api.SettingsBody
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.RestartRequired) != 0 {
		t.Fatal("complete auth PATCH advertised restart")
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool { c, e := v.AuthControls(); return e == nil && match(c) })
}

// A deliberate canonical change through the real durable source, for held-view
// and browser fixtures that do not own an administrator HTTP token.
func authReloadStage(t *testing.T, f *servingHTTPFixture, raw string, match func(config.AuthControls) bool) {
	t.Helper()
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		if settings.Spec.Settings == nil {
			settings.Spec.Settings = map[string]extensions.JSON{}
		}
		settings.Spec.Settings["auth"] = extensions.JSON{Raw: []byte(raw)}
		_, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool { c, e := v.AuthControls(); return e == nil && match(c) })
}

type authReloadTLSClient struct {
	client *http.Client
	base   string
}

func (c authReloadTLSClient) call(t *testing.T, method, path, body, token string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, c.base+api.BasePath+path, strings.NewReader(body))
	if err != nil {
		t.Fatal("auth reload TLS request construction failed")
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if csrf != "" {
		r.Header.Set(auth.CSRFHeader, csrf)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	response, err := c.client.Do(r)
	if err != nil {
		t.Fatal("auth reload TLS transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	w := httptest.NewRecorder()
	for key, values := range response.Header {
		w.Header()[key] = values
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(response.Body, 1<<20)); err != nil {
		t.Fatal("auth reload TLS response read failed")
	}
	return w
}

func TestServingHTTPAuthControlsLiveTLSAndOriginalDeadlines(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	authReloadHTTP(t, f)
	tlsServer := httptest.NewTLSServer(f.server)
	t.Cleanup(tlsServer.Close)
	c := authReloadTLSClient{client: tlsServer.Client(), base: tlsServer.URL}
	login := func() (*http.Cookie, api.SessionInfo) {
		w := c.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, "")
		expectServingHTTPStatus(t, w, 200)
		var info api.SessionInfo
		if json.Unmarshal(w.Body.Bytes(), &info) != nil {
			t.Fatal("auth reload session decode failed")
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("missing single enrolled cookie")
		}
		return cookies[0], info
	}
	old, oldInfo := login()
	if old.Name != "__Host-kudurru_session" || !old.Secure {
		t.Fatal("auto policy lost actual TLS cookie protection")
	}
	authReloadPatch(t, f, `{"auth":{"session":{"idleTimeout":"2m","lifetime":"7m"},"tokens":{"maxLifetime":"5m"}}}`, func(v config.AuthControls) bool {
		return v.SessionLifetime == 7*time.Minute && v.SessionIdleTimeout == 2*time.Minute && v.TokenMaxLifetime == 5*time.Minute
	})
	current, info := login()
	if info.ExpiresAt == nil || info.IdleExpiresAt == nil || info.ExpiresAt.Sub(*info.IdleExpiresAt) != 5*time.Minute {
		t.Fatal("new enrollment ignored live durations")
	}
	read := c.call(t, "GET", "/auth/me", "", "", old, "")
	expectServingHTTPStatus(t, read, 200)

	records := c.call(t, "GET", "/auth/sessions", "", "", old, "")
	expectServingHTTPStatus(t, records, 200)
	var retained api.AuthList[api.SessionRecordInfo]
	if json.Unmarshal(records.Body.Bytes(), &retained) != nil {
		t.Fatal("session inventory decode failed")
	}
	found := false
	for _, row := range retained.Items {
		if row.Current {
			found = true
			if !row.ExpiresAt.Equal(*oldInfo.ExpiresAt) || !row.IdleExpiresAt.Equal(*oldInfo.IdleExpiresAt) {
				t.Fatal("reload changed an earlier session deadline")
			}
		}
	}
	if !found {
		t.Fatal("original session missing from inventory")
	}

	expectServingHTTPStatus(t, c.call(t, "POST", "/auth/tokens", `{"expiresIn":"6m"}`, f.token, nil, ""), 400)
	token := c.call(t, "POST", "/auth/tokens", `{}`, f.token, nil, "")
	expectServingHTTPStatus(t, token, 201)
	var created api.CreatedToken
	if json.Unmarshal(token.Body.Bytes(), &created) != nil || created.ExpiresAt == nil || created.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		t.Fatal("new token ignored live cap")
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	authReloadPatch(t, f, `{"auth":{"localLogin":"disabled"}}`, func(v config.AuthControls) bool { return v.LocalLogin == config.LocalLoginDisabled })
	expectServingHTTPStatus(t, c.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, ""), 403)
	expectServingHTTPStatus(t, c.call(t, "GET", "/auth/me", "", "", current, ""), 200)
	authReloadPatch(t, f, `{"auth":{"localLogin":"enabled","session":{"secure":"never"}}}`, func(v config.AuthControls) bool {
		return v.LocalLogin == config.LocalLoginEnabled && v.SessionSecure == "never"
	})
	expectServingHTTPStatus(t, c.call(t, "GET", "/auth/me", "", "", old, ""), 401)
	bare, bareInfo := login()
	if bare.Name != "kudurru_session" || bare.Secure {
		t.Fatal("never policy retained startup/TLS secure cookie")
	}
	// Cross-origin/CSRF refusal remains independent of live cookie selection.
	expectServingHTTPStatus(t, c.call(t, "POST", "/auth/logout", "", "", bare, ""), 403)
	logout := c.call(t, "POST", "/auth/logout", "", "", bare, bareInfo.CSRF)
	expectServingHTTPStatus(t, logout, 204)
	for _, ck := range logout.Result().Cookies() {
		if ck.Secure || strings.HasPrefix(ck.Name, "__Host-") || ck.MaxAge != -1 {
			t.Fatal("logout cookie clear ignored original mode")
		}
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	schema := c.call(t, "GET", "/settings/_schema", "", f.token, nil, "")
	// Schema help legitimately contains the public word "evidence"; the
	// credential-response helper's blanket text check is not appropriate here.
	if schema.Code != http.StatusOK {
		t.Fatalf("schema HTTP status: %d", schema.Code)
	}
	var keys api.List[api.SettingKey]
	if json.Unmarshal(schema.Body.Bytes(), &keys) != nil {
		t.Fatal("schema decode failed")
	}
	count := 0
	for _, k := range keys.Items {
		if config.AuthControlHot(k.Path) {
			count++
			if k.RestartRequired {
				t.Fatal("complete schema advertised restart")
			}
		} else if strings.HasPrefix(k.Path, "auth.") && !k.RestartRequired {
			t.Fatal("host auth authority advertised hot")
		}
	}
	if count != 5 {
		t.Fatal("complete schema omitted auth controls")
	}
	expectServingHTTPStatus(t, c.call(t, "PATCH", "/settings", `{"auth":{"providerResources":{"systemTrust":false}}}`, f.token, nil, ""), 400)
}

func TestServingHTTPAuthCookieCommittedEnrollmentMismatch(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	authReloadHTTP(t, f)
	w := httptest.NewRecorder()
	called := false
	f.h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		var enrollment security.Enrollment
		issued := 0
		err := f.s.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			issued++
			observed := f.h.now()
			var err error
			enrollment, err = security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "root-command", Value: "initial-password"}, observed)
			if err != nil {
				return err
			}
			return f.h.enrollmentClock(ctx, tx, enrollment, observed)
		})
		if err != nil {
			t.Fatal("controlled enrollment command failed")
		}
		authReloadStage(t, f, `{"session":{"secure":"always"}}`, func(c config.AuthControls) bool { return c.SessionSecure == "always" })
		if err := f.h.sealEnrollment(w, r, enrollment); !errors.Is(err, serving.ErrSuperseded) {
			t.Fatal("committed response adopted replacement cookie policy")
		}
		if issued != 1 || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatal("policy refusal retried issuance or disclosed a cookie")
		}
	})).ServeHTTP(w, f.request())
	if !called {
		t.Fatal("controlled enrollment did not reach captured handler")
	}
}

func TestServingHTTPAuthCookieOriginalViewAndPostcommitRefusal(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	authReloadHTTP(t, f)
	r := f.request()
	called := false
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		original, err := f.h.cookiePolicy(r, "")
		if err != nil || original.Secure != session.SecureAuto {
			t.Fatal("original policy missing")
		}
		authReloadStage(t, f, `{"session":{"secure":"always"}}`, func(c config.AuthControls) bool { return c.SessionSecure == "always" })
		retained, err := f.h.cookiePolicy(r, "")
		if err != nil || retained != original {
			t.Fatal("successor repaired original request policy")
		}
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			t.Fatal("successor policy publication missing")
		}
		defer lease.Release()
		owner, _ := lease.Bundle().Component("auth")
		if err := f.h.checkCookiePublication(r, owner.(*servingauth.View)); !errors.Is(err, serving.ErrSuperseded) {
			t.Fatal("postcommit adopted successor cookie policy")
		}
		invoked := false
		if err := f.s.currentAuthCommand(r.Context(), func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { invoked = true; return nil }); err == nil || invoked {
			t.Fatal("old lease admitted a command after policy publication")
		}
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("original proof did not reach held handler")
	}
}

func TestServingHTTPAuthCookieBrowserPolicyAndPendingRefusal(t *testing.T) {
	f := newServingBrowserFixture(t)
	authReloadHTTP(t, f.servingHTTPFixture)
	authReloadStage(t, f.servingHTTPFixture, `{"session":{"secure":"never"}}`, func(c config.AuthControls) bool { return c.SessionSecure == "never" })
	pending, callback := f.begin(t, "/")
	authReloadStage(t, f.servingHTTPFixture, `{"session":{"secure":"always"}}`, func(c config.AuthControls) bool { return c.SessionSecure == "always" })
	before := f.op.requests.Load()
	w := f.callback(t, callback, pending)
	expectServingHTTPStatus(t, w, 401)
	if f.op.requests.Load() != before || len(f.security(t).ExternalSessions) != 0 {
		t.Fatal("old cookie alias repaired pending flow or reached exchange")
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name != "__Host-kudurru_session_oidc" || !ck.Secure || ck.MaxAge != -1 {
			t.Fatal("pending failure clear used startup cookie mode")
		}
	}
	// A fresh flow uses actual TLS for both API and OP exchange.
	server := httptest.NewTLSServer(f.server)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := authReloadTLSClient{client: client, base: server.URL}
	w = c.call(t, "GET", "/auth/oidc/corp/start?next=/", "", "", nil, "")
	expectServingHTTPStatus(t, w, 302)
	pending = logoutCookieFrom(t, w, "__Host-kudurru_session_oidc")
	if !pending.Secure || pending.MaxAge <= 0 {
		t.Fatal("fresh preauth ignored new cookie mode")
	}
	callback = f.op.Complete(t, w.Header().Get("Location"))
	u, err := url.Parse(callback)
	if err != nil {
		t.Fatal("callback construction failed")
	}
	w = c.call(t, "GET", strings.TrimPrefix(u.RequestURI(), api.BasePath), "", "", pending, "")
	expectServingHTTPStatus(t, w, 302)
	enrolled := logoutCookieFrom(t, w, "__Host-kudurru_session")
	hint := logoutCookieFrom(t, w, "__Host-kudurru_session_oidc_hint")
	for _, ck := range w.Result().Cookies() {
		if !ck.Secure || !strings.HasPrefix(ck.Name, "__Host-") {
			t.Fatal("browser enrollment/hint used startup policy")
		}
	}
	me := c.call(t, "GET", "/auth/me", "", "", enrolled, "")
	expectServingHTTPStatus(t, me, 200)
	var info api.SessionInfo
	if json.Unmarshal(me.Body.Bytes(), &info) != nil {
		t.Fatal("browser session decode failed")
	}
	w = f.logoutCall(t, "POST", "/auth/logout/browser", []*http.Cookie{enrolled, hint}, info.CSRF)
	expectServingHTTPStatus(t, w, 200)
	continuation := logoutCookieFrom(t, w, "__Host-kudurru_session_oidc_logout")
	for _, ck := range w.Result().Cookies() {
		if !ck.Secure || !strings.HasPrefix(ck.Name, "__Host-") {
			t.Fatal("logout clear/continuation used startup mode")
		}
	}
	w = f.logoutCall(t, "GET", "/auth/oidc/corp/logout", []*http.Cookie{continuation}, "")
	expectServingHTTPStatus(t, w, 302)
	state := logoutCookieFrom(t, w, "__Host-kudurru_session_oidc_logout_state")
	if !state.Secure {
		t.Fatal("logout state lost live secure mode")
	}
	opClient := f.op.Server.Client()
	opClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := opClient.Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatal("TLS OP logout failed")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	u, err = url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal("logout callback construction failed")
	}
	w = f.logoutCall(t, "GET", strings.TrimPrefix(u.RequestURI(), api.BasePath), []*http.Cookie{state}, "")
	expectServingHTTPStatus(t, w, 302)
	for _, ck := range w.Result().Cookies() {
		if ck.Name != "__Host-kudurru_session_oidc_logout_state" || !ck.Secure || ck.MaxAge != -1 {
			t.Fatal("logout callback clear used startup policy")
		}
	}
}

func TestServingHTTPAuthCookieFactorAndPasswordClears(t *testing.T) {
	for _, operation := range []string{"confirm", "disable", "recover"} {
		t.Run(operation, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			authReloadHTTP(t, f)
			authReloadPatch(t, f, `{"auth":{"session":{"secure":"always"}}}`, func(c config.AuthControls) bool { return c.SessionSecure == "always" })
			cookie, info := f.login(t)
			var result *httptest.ResponseRecorder
			switch operation {
			case "confirm":
				setup := factorSetupHTTP(t, f, cookie, info.CSRF, "")
				result = f.call(t, "POST", "/auth/totp/confirm", factorBody(t, api.TOTPConfirmRequest{CurrentPassword: "initial-password", Challenge: setup.Challenge, Code: factorCode(t, setup.Secret, time.Now().Add(-30*time.Second))}), "", cookie, info.CSRF)
			case "disable":
				// Already disabled is a real authorized no-op, with identical
				// original cookie-clearing policy and current writer checks.
				result = f.call(t, "POST", "/auth/totp/disable", `{"currentPassword":"initial-password"}`, "", cookie, info.CSRF)
			case "recover":
				u := localUsersRead(t, f.s.opts.Store, "root-command")
				result = f.call(t, "POST", "/iam/users/root-command/totp/recover", factorBody(t, api.TOTPRecoverRequest{UID: string(u.UID)}), "", cookie, info.CSRF)
			}
			expectServingHTTPStatus(t, result, 204)
			factorCleared(t, result)
			ck := result.Result().Cookies()[0]
			if ck.Name != "__Host-kudurru_session" || !ck.Secure {
				t.Fatal("credential mutation clear used startup policy")
			}
		})
	}
}

func TestServingHTTPAuthCookieManagedSelfReset(t *testing.T) {
	f := newServingBrowserFixture(t)
	authReloadHTTP(t, f.servingHTTPFixture)
	authReloadStage(t, f.servingHTTPFixture, `{"session":{"secure":"always"}}`, func(c config.AuthControls) bool { return c.SessionSecure == "always" })
	hash, err := f.h.opts.Hasher.Hash(t.Context(), "initial-password")
	if err != nil {
		t.Fatal("self reset fixture password hashing failed")
	}
	localUsersStage(t, f.servingHTTPFixture, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "reload-admin"}, Spec: internalv1.LocalUserSpec{PasswordHash: hash}}, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "reload-admin", Namespace: "apps"}, Spec: public.RoleBindingSpec{Role: "admin", Scope: public.BindingScope{Kind: "cluster"}, Subjects: []public.BindingSubject{{Kind: "User", Name: "reload-admin"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	cookie, info := localUsersLogin(t, f.servingHTTPFixture, "reload-admin", "initial-password", "")
	u := localUsersRead(t, f.s.opts.Store, "reload-admin")
	w := f.call(t, "POST", "/iam/users/reload-admin/password", localUsersResetBody(t, u), "", cookie, info.CSRF)
	expectServingHTTPStatus(t, w, 204)
	factorCleared(t, w)
	ck := w.Result().Cookies()[0]
	if ck.Name != "__Host-kudurru_session" || !ck.Secure {
		t.Fatal("managed self reset clear used startup cookie mode")
	}
}
