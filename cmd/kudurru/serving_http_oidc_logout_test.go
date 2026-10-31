package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func logoutCookieFrom(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge > 0 {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || len(c.Value) > 3800 {
				t.Fatal("private cookie contract")
			}
			return c
		}
	}
	t.Fatal("missing private logout cookie")
	return nil
}
func (f *servingBrowserFixture) logoutCall(t *testing.T, method, path string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "https://kudurru.test/api/v1"+path, nil)
	r.RemoteAddr = "192.0.2.24:1"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if csrf != "" {
		r.Header.Set(auth.CSRFHeader, csrf)
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}
func (f *servingBrowserFixture) logoutLogin(t *testing.T) (*http.Cookie, *http.Cookie, string) {
	t.Helper()
	pending, callback := f.begin(t, "/")
	w := f.callback(t, callback, pending)
	expectServingHTTPStatus(t, w, 302)
	session := logoutCookieFrom(t, w, "kudurru_session")
	hint := logoutCookieFrom(t, w, "kudurru_session_oidc_hint")
	me := f.call(t, "GET", "/auth/me", "", "", session, "")
	expectServingHTTPStatus(t, me, 200)
	var info api.Me
	if json.Unmarshal(me.Body.Bytes(), &info) != nil || info.CSRF == "" {
		t.Fatal("missing original CSRF")
	}
	return session, hint, info.CSRF
}
func (f *servingBrowserFixture) committedLogout(t *testing.T, session, hint *http.Cookie, csrf string) (*httptest.ResponseRecorder, api.BrowserLogoutResponse) {
	t.Helper()
	w := f.logoutCall(t, "POST", "/auth/logout/browser", []*http.Cookie{session, hint}, csrf)
	expectServingHTTPStatus(t, w, 200)
	var result api.BrowserLogoutResponse
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("invalid logout response")
	}
	state := f.security(t)
	if len(state.ExternalSessions) != 1 || !state.ExternalSessions[0].Revoked {
		t.Fatal("remote logout preceded committed local revocation")
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "kudurru_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("local session cookie retained")
	}
	f.op.mu.Lock()
	rawToken := f.op.lastToken
	f.op.mu.Unlock()
	if strings.Contains(w.Body.String(), rawToken) || strings.Contains(w.Body.String(), "state") {
		t.Fatal("private token or state reached API JSON")
	}
	return w, result
}
func TestServingHTTPBrowserLogoutActualTLSRoundTrip(t *testing.T) {
	for _, providerError := range []bool{false, true} {
		name := "success"
		if providerError {
			name = "provider-error"
		}
		t.Run(name, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			session, hint, csrf := f.logoutLogin(t)
			f.op.logoutFailure.Store(providerError)
			w, result := f.committedLogout(t, session, hint, csrf)
			if result.ContinuePath != "/api/v1/auth/oidc/corp/logout" {
				t.Fatal("unsafe continuation path")
			}
			continuation := logoutCookieFrom(t, w, "kudurru_session_oidc_logout")
			next := f.logoutCall(t, "GET", strings.TrimPrefix(result.ContinuePath, "/api/v1"), []*http.Cookie{continuation}, "")
			expectServingHTTPStatus(t, next, 302)
			if next.Body.Len() != 0 || next.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("private redirect response contract")
			}
			callbackState := logoutCookieFrom(t, next, "kudurru_session_oidc_logout_state")
			client := f.op.Server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			// Real local TLS request, not a mocked provider/client response.
			response, err := client.Get(next.Header().Get("Location"))
			if err != nil {
				t.Fatal("local TLS end-session request failed")
			}
			_ = response.Body.Close()
			if response.StatusCode != 302 || f.op.logoutRequests.Load() != 1 {
				t.Fatal("OP refused actual signed ID-token-hint profile")
			}
			target, _ := url.Parse(response.Header.Get("Location"))
			returned := f.logoutCall(t, "GET", strings.TrimPrefix(target.RequestURI(), "/api/v1"), []*http.Cookie{callbackState}, "")
			expectServingHTTPStatus(t, returned, 302)
			if returned.Header().Get("Location") != "/" || returned.Body.Len() != 0 {
				t.Fatal("provider error or redirect escaped safe local return")
			}
			for _, c := range returned.Result().Cookies() {
				if c.Name == "kudurru_session" && c.MaxAge >= 0 {
					t.Fatal("logout callback issued a session")
				}
			}
			// Revoked cookie never authenticates again, irrespective of OP outcome.
			awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
				_, err := v.CookieSession(session.Value, time.Now())
				return err != nil
			})
			expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", session, ""), 401)
			// Browser deletion enforces normal callback consumption; no cookie refuses.
			expectServingHTTPStatus(t, f.logoutCall(t, "GET", strings.TrimPrefix(target.RequestURI(), "/api/v1"), nil, ""), 401)
		})
	}
}
func TestServingHTTPBrowserLogoutCSRFAndOriginalWriterRefusals(t *testing.T) {
	for _, mode := range []string{"csrf", "writer"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			session, hint, csrf := f.logoutLogin(t)
			if mode == "csrf" {
				csrf = "wrong"
			} else {
				f.stopWriter()
			}
			before := f.security(t).Epoch
			w := f.logoutCall(t, "POST", "/auth/logout/browser", []*http.Cookie{session, hint}, csrf)
			if w.Code < 400 {
				t.Fatal("logout bypassed original CSRF or current writer gates")
			}
			if f.security(t).Epoch != before || f.security(t).ExternalSessions[0].Revoked {
				t.Fatal("refused request changed enrollment")
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == "kudurru_session_oidc_logout" && c.MaxAge > 0 {
					t.Fatal("refused request issued continuation")
				}
			}
		})
	}
}
func TestServingHTTPBrowserLogoutExpiredOrMismatchedHintStaysLocal(t *testing.T) {
	for _, mode := range []string{"expired", "mismatched", "missing"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			if mode == "expired" {
				f.op.mu.Lock()
				f.op.mode = "logout-short"
				f.op.mu.Unlock()
			}
			session, hint, csrf := f.logoutLogin(t)
			if mode == "expired" {
				time.Sleep(2100 * time.Millisecond)
			}
			if mode == "mismatched" {
				hint.Value = "untrusted"
			}
			if mode == "missing" {
				hint.Value = ""
			}
			_, result := f.committedLogout(t, session, hint, csrf)
			if result.ContinuePath != "" || f.op.logoutRequests.Load() != 0 {
				t.Fatal("invalid hint gained provider redirect")
			}
		})
	}
}
func TestServingHTTPBrowserLogoutStateAndRedirectRefusals(t *testing.T) {
	f := newServingBrowserFixture(t)
	session, hint, csrf := f.logoutLogin(t)
	w, _ := f.committedLogout(t, session, hint, csrf)
	continuation := logoutCookieFrom(t, w, "kudurru_session_oidc_logout")
	expectServingHTTPStatus(t, f.logoutCall(t, "GET", "/auth/oidc/corp/logout?next=https://evil.example", []*http.Cookie{continuation}, ""), 401)
	next := f.logoutCall(t, "GET", "/auth/oidc/corp/logout", []*http.Cookie{continuation}, "")
	expectServingHTTPStatus(t, next, 302)
	state := logoutCookieFrom(t, next, "kudurru_session_oidc_logout_state")
	endpoint, _ := url.Parse(next.Header().Get("Location"))
	correct := endpoint.Query().Get("state")
	for _, path := range []string{"/auth/oidc/corp/logout/callback?state=wrong", "/auth/oidc/corp/logout/callback?state=" + correct + "&state=" + correct, "/auth/oidc/other/logout/callback?state=" + correct, "/auth/oidc/corp/logout/callback?state=" + correct + "&next=https://evil.example", "/auth/oidc/corp/logout/callback?state=" + correct + "&error_uri=https://evil.example"} {
		refused := f.logoutCall(t, "GET", path, []*http.Cookie{state}, "")
		expectServingHTTPStatus(t, refused, 401)
		if refused.Header().Get("Location") != "" {
			t.Fatal("untrusted callback redirected")
		}
	}
	// Phase separation: an enrolled hint or callback cannot start a new remote logout.
	wrongPhase := *continuation //nolint:gosec // G124: request-only fixture cookie copy, never a response cookie.
	wrongPhase.Value = hint.Value
	expectServingHTTPStatus(t, f.logoutCall(t, "GET", "/auth/oidc/corp/logout", []*http.Cookie{&wrongPhase}, ""), 401)
}
func TestServingHTTPBrowserLogoutCredentialRotationAfterLocalCommit(t *testing.T) {
	f := newServingBrowserFixture(t)
	session, hint, csrf := f.logoutLogin(t)
	w, _ := f.committedLogout(t, session, hint, csrf)
	continuation := logoutCookieFrom(t, w, "kudurru_session_oidc_logout")
	// Actual installed credential bytes change after local commit. The redirect
	// checks the original resolver/fingerprint directly, without a refresh wait.
	path := f.s.opts.Config.Auth.IdentityProviders[0]["clientSecretFile"].(string)
	if err := os.WriteFile(path, []byte("rotated-test-only-secret"), 0600); err != nil {
		t.Fatal("fixture credential rotation failed")
	}
	next := f.logoutCall(t, "GET", "/auth/oidc/corp/logout", []*http.Cookie{continuation}, "")
	if next.Code != 302 || next.Header().Get("Location") != "/" {
		t.Fatal("replacement authority redirected original token hint")
	}
	if f.op.logoutRequests.Load() != 0 || !f.security(t).ExternalSessions[0].Revoked {
		t.Fatal("rotation undid local revoke or contacted OP")
	}
}

func TestServingHTTPBrowserLogoutUnsupportedDiscoveryAndLocalCompatibility(t *testing.T) {
	f := newServingBrowserFixtureConfigured(t, func(p *servingBrowserOP) { p.mu.Lock(); p.omitLogout = true; p.mu.Unlock() })
	pending, callback := f.begin(t, "/")
	loggedIn := f.callback(t, callback, pending)
	expectServingHTTPStatus(t, loggedIn, 302)
	for _, c := range loggedIn.Result().Cookies() {
		if c.Name == "kudurru_session_oidc_hint" && c.MaxAge > 0 {
			t.Fatal("missing end-session endpoint retained a hint")
		}
	}
	session := logoutCookieFrom(t, loggedIn, "kudurru_session")
	me := f.call(t, "GET", "/auth/me", "", "", session, "")
	expectServingHTTPStatus(t, me, 200)
	var info api.Me
	_ = json.Unmarshal(me.Body.Bytes(), &info)
	// Existing CLI and old callers retain local204 and the same current gates.
	local := f.logoutCall(t, "POST", "/auth/logout", []*http.Cookie{session}, info.CSRF)
	expectServingHTTPStatus(t, local, 204)
	if !f.security(t).ExternalSessions[0].Revoked || f.op.logoutRequests.Load() != 0 {
		t.Fatal("local compatibility lost revocation or contacted provider")
	}
}

func TestServingHTTPBrowserLogoutCannotMixTwoEnrolledSessions(t *testing.T) {
	f := newServingBrowserFixture(t)
	_, firstHint, _ := f.logoutLogin(t)
	firstID := f.security(t).ExternalSessions[0].ID
	secondSession, _, secondCSRF := f.logoutLogin(t)
	result := f.logoutCall(t, "POST", "/auth/logout/browser", []*http.Cookie{secondSession, firstHint}, secondCSRF)
	expectServingHTTPStatus(t, result, 200)
	var out api.BrowserLogoutResponse
	_ = json.Unmarshal(result.Body.Bytes(), &out)
	state := f.security(t)
	if out.ContinuePath != "" || len(state.ExternalSessions) != 2 {
		t.Fatal("mixed original hint/session issued redirect")
	}
	for _, record := range state.ExternalSessions {
		if record.ID == firstID && record.Revoked || record.ID != firstID && !record.Revoked {
			t.Fatal("mixed hint revoked the wrong enrollment")
		}
	}
}

func TestServingHTTPBrowserLogoutUnavailableOPKeepsLocalRevocation(t *testing.T) {
	f := newServingBrowserFixture(t)
	session, hint, csrf := f.logoutLogin(t)
	w, _ := f.committedLogout(t, session, hint, csrf)
	continuation := logoutCookieFrom(t, w, "kudurru_session_oidc_logout")
	next := f.logoutCall(t, "GET", "/auth/oidc/corp/logout", []*http.Cookie{continuation}, "")
	expectServingHTTPStatus(t, next, 302)
	// Once the browser leaves, OP downtime cannot be detected by the RP. Its
	// durable local revocation and cookie clearing must already be complete.
	client := f.op.Server.Client()
	client.Timeout = time.Second
	f.op.Server.Close()
	response, err := client.Get(next.Header().Get("Location"))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("closed local OP unexpectedly reachable")
	}
	if !f.security(t).ExternalSessions[0].Revoked || f.op.logoutRequests.Load() != 0 {
		t.Fatal("unreachable OP undid revocation or request was retried")
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, err := v.CookieSession(session.Value, time.Now())
		return err != nil
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", session, ""), 401)
}

func TestServingHTTPBrowserLogoutLocalAccountHasNoRemoteAuthority(t *testing.T) {
	f := newServingHTTPFixture(t, true)
	cookie, info := f.login(t)
	w := f.call(t, "POST", "/auth/logout/browser", "", "", cookie, info.CSRF)
	expectServingHTTPStatus(t, w, 200)
	var out api.BrowserLogoutResponse
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ContinuePath != "" {
		t.Fatal("local account acquired provider redirect authority")
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, err := v.CookieSession(cookie.Value, f.s.opts.Now())
		return err != nil
	})
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/logout/browser", "", "", cookie, info.CSRF), 401)
}
