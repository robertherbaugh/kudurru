package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type servingHTTPFixture struct {
	*servingCommandFixture
	h      *servingHTTP
	server http.Handler
}

func newServingHTTPFixture(t *testing.T, mustChange bool) *servingHTTPFixture {
	t.Helper()
	f := &servingHTTPFixture{servingCommandFixture: newServingCommandFixture(t)}
	f.s.opts.Now = time.Now
	hasher := users.NewHasher(1)
	hash, err := hasher.Hash(t.Context(), "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var u internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &u); err != nil {
			return err
		}
		u.Spec.PasswordHash = hash
		u.Spec.MustChangePassword = mustChange
		_, err := tx.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		objects, e := v.Objects("LocalUser")
		if e != nil {
			return false
		}
		for _, o := range objects {
			u := o.(*internalv1.LocalUser)
			if u.Name == "root-command" {
				return u.Spec.PasswordHash == hash
			}
		}
		return false
	})
	f.h, err = newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: hasher, Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, CanonicalSettings: f.s, Authenticate: f.h.Middleware, AuthHandler: f.h.Handler(), Authorizer: f.h.Authorizer(), Roles: f.h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.server = srv
	return f
}
func awaitServingHTTPView(t *testing.T, s *servingRuntime, predicate func(*servingauth.View) bool) {
	t.Helper()
	awaitServingRuntime(t, s, func() bool {
		l, e := s.publisher.CaptureHTTP(t.Context())
		if e != nil {
			return false
		}
		defer l.Release()
		value, ok := l.Bundle().Component("auth")
		v, typed := value.(*servingauth.View)
		return ok && typed && predicate(v)
	})
}
func (f *servingHTTPFixture) call(t *testing.T, method, path, body, token string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://kudurru.test/api/v1"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.20:3131"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set(auth.CSRFHeader, csrf)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}
func expectServingHTTPStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status: got %d, want %d", w.Code, status)
	}
	if strings.Contains(w.Body.String(), "secretHash") || strings.Contains(w.Body.String(), "passwordHash") || strings.Contains(w.Body.String(), "csrfHash") || strings.Contains(w.Body.String(), "evidence") || strings.Contains(w.Body.String(), "totpSecret") {
		t.Fatal("private owner material reached HTTP")
	}
}
func (f *servingHTTPFixture) login(t *testing.T) (*http.Cookie, api.SessionInfo) {
	t.Helper()
	w := f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, "")
	expectServingHTTPStatus(t, w, 200)
	var out api.SessionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || out.CSRF == "" || !cookies[0].HttpOnly {
		t.Fatal("missing enrolled cookie/CSRF")
	}
	return cookies[0], out
}
func TestServingHTTPDurableLoginTokensSessionsLogout(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	status := f.call(t, "GET", "/auth/status", "", "", nil, "")
	expectServingHTTPStatus(t, status, 200)
	cookie, sessionInfo := f.login(t)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 200)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/tokens", `{}`, "", cookie, ""), 403)
	created := f.call(t, "POST", "/auth/tokens", `{"description":"child","expiresIn":"1m","scopes":["viewer"]}`, "", cookie, sessionInfo.CSRF)
	expectServingHTTPStatus(t, created, 201)
	var child api.CreatedToken
	if err := json.Unmarshal(created.Body.Bytes(), &child); err != nil {
		t.Fatal(err)
	}
	if child.Token == "" || child.Owner.UID == "" || child.ExpiresAt == nil {
		t.Fatal("missing committed token")
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, e := v.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: child.Token}, time.Now())
		return e == nil
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/tokens", "", child.Token, nil, ""), 200)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/tokens", `{"scopes":["admin"]}`, child.Token, nil, ""), 403)
	grand := f.call(t, "POST", "/auth/tokens", `{}`, child.Token, nil, "")
	expectServingHTTPStatus(t, grand, 201)
	var grandchild api.CreatedToken
	if err := json.Unmarshal(grand.Body.Bytes(), &grandchild); err != nil {
		t.Fatal(err)
	}
	if grandchild.ExpiresAt == nil || grandchild.ExpiresAt.After(*child.ExpiresAt) || len(grandchild.Scopes) != 1 || grandchild.Scopes[0] != "viewer" {
		t.Fatal("child broadened scope or expiry")
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/sessions", "", "", cookie, ""), 200)
	for _, route := range []string{"users", "serviceaccounts", "rolebindings", "identityproviders"} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/iam/"+route, "", f.token, nil, ""), 200)
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/iam/users", "", child.Token, nil, ""), 403)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/auth/tokens/"+child.ID, "", f.token, nil, ""), 204)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, e := v.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: child.Token}, time.Now())
		return e != nil
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", child.Token, nil, ""), 401)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/logout", "", "", cookie, sessionInfo.CSRF), 204)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool { _, e := v.CookieSession(cookie.Value, time.Now()); return e != nil })
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
}
func TestServingHTTPPasswordChangeAndMustChange(t *testing.T) {
	f := newServingHTTPFixture(t, true)
	cookie, info := f.login(t)
	if !info.MustChangePassword || len(info.Permissions) != 0 {
		t.Fatal("must-change account retained permissions")
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/tokens", `{}`, "", cookie, info.CSRF), 403)
	changed := f.call(t, "POST", "/auth/password", `{"currentPassword":"initial-password","newPassword":"replacement-password","revokeTokens":true}`, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, changed, 200)
	var out api.SessionInfo
	if err := json.Unmarshal(changed.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	cookies := changed.Result().Cookies()
	if len(cookies) != 1 || out.MustChangePassword || out.CSRF == "" || out.CSRF == info.CSRF {
		t.Fatal("password change did not issue new enrollment")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", f.token, nil, ""), 401)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookies[0], ""), 200)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, ""), 401)
}
func TestServingHTTPStrictBodiesOriginAndLockout(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	for _, body := range []string{`{"username":"root-command","password":"initial-password","unknown":true}`, `{} {}`, `[]`, `null`} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", body, "", nil, ""), 400)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", strings.Repeat("x", 65537), "", nil, ""), 413)
	r := httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/login", bytes.NewBufferString(`{"username":"root-command","password":"initial-password"}`))
	r.Header.Set("Origin", "https://attacker.invalid")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	expectServingHTTPStatus(t, w, 403)
	for range users.DefaultIPLimit.Failures {
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"wrong"}`, "", nil, ""), 401)
	}
	w = f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, "")
	expectServingHTTPStatus(t, w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing lockout retry")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/oidc/corp/start", "", "", nil, ""), 503)
}

func TestServingHTTPSessionRevokeAndAccountIncarnation(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	listed := f.call(t, "GET", "/auth/sessions", "", "", cookie, "")
	expectServingHTTPStatus(t, listed, 200)
	var records api.AuthList[api.SessionRecordInfo]
	if err := json.Unmarshal(listed.Body.Bytes(), &records); err != nil || len(records.Items) != 1 || !records.Items[0].Current {
		t.Fatal("missing current durable session", err)
	}
	// A UID is mandatory: another account's enrollment never becomes own through
	// display names, a fabricated path, or the principal stored in middleware.
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/auth/sessions/not-owned-session", "", "", cookie, info.CSRF), 404)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/auth/sessions/"+records.Items[0].ID, "", "", cookie, info.CSRF), 204)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool { _, err := v.CookieSession(cookie.Value, time.Now()); return err != nil })
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/sessions", "", "", cookie, ""), 401)
	// Delete/recreate the same username with a new UID. An old bearer and cookie
	// must both deny; there is no legacy store or same-name adoption path.
	freshCookie, _ := f.login(t)
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var u internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &u); err != nil {
			return err
		}
		if err := tx.Delete(ctx, store.KeyOf(&u), store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		u.UID = ""
		u.ResourceVersion = ""
		u.CreationTimestamp = metav1.Time{}
		_, err := tx.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, err := v.CookieSession(freshCookie.Value, time.Now())
		return err != nil
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", freshCookie, ""), 401)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", f.token, nil, ""), 401)
}
