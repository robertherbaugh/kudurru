package oidc

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

// runSuite is the end-to-end contract every IdP under test must pass. The
// subtests share one harness (and one IdP process) and run sequentially;
// each resets the clock, sessionsValidAfter and the keys.
func runSuite(t *testing.T, h *harness, idp testIDP) {
	reset := func(t *testing.T) {
		h.t = t
		h.clk.Reset()
		h.validAfter.Clear()
		h.configure(idp, keyA, nil)
	}

	t.Run("login sets session cookie and /me shows principal and roles", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		authURL := b.startLogin("/zones")
		if pre := b.cookie("kudurru_oidc_" + idp.id); pre != nil {
			t.Fatalf("pre-auth cookie visible outside its callback path: %+v", pre)
		}
		resp, body := b.finish(b.authenticateAtIDP(authURL, userAdmin))
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/zones" {
			t.Fatalf("callback: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		var sc *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == session.DefaultCookieName {
				sc = c
			}
		}
		if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Secure || sc.Path != "/" {
			t.Fatalf("session cookie attributes: %+v", sc)
		}
		if sc.MaxAge != int(session.DefaultLifetime.Seconds()) {
			t.Errorf("Max-Age = %d", sc.MaxAge)
		}
		status, me := b.me()
		if status != http.StatusOK {
			t.Fatalf("/me: %d", status)
		}
		wantRoles := "admin,zone-admin@zone:home-example-com"
		if got := strings.Join(me.Roles, ","); got != wantRoles {
			t.Errorf("roles_snapshot = %s, want %s", got, wantRoles)
		}
		key, err := ParseIdentityKey(me.Principal.ID)
		if err != nil || key.Provider != idp.id || key.Issuer != idp.issuer || key.Subject == "" {
			t.Errorf("principal id %q -> %+v, %v", me.Principal.ID, key, err)
		}
		if me.Principal.Name != idp.id+":admin" || me.Principal.Kind != auth.KindUser || me.Principal.AuthMethod != session.AuthMethodCookie {
			t.Errorf("principal = %+v", me.Principal)
		}
		if me.CSRF == "" || me.IDP != idp.id {
			t.Errorf("csrf %q idp %q", me.CSRF, me.IDP)
		}
		if len(me.Permissions["cluster"]) == 0 || len(me.Permissions["zone:home-example-com"]) == 0 {
			t.Errorf("permission matrix = %v", me.Permissions)
		}
		t.Logf("session cookie for %s (2 grants): %d bytes; principal id %d bytes", userAdmin, len(sc.Name)+1+len(sc.Value), len(me.Principal.ID))
	})

	t.Run("user without a matching binding is refused", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		resp, body := b.login(userNobody, "/")
		wantLoginError(t, resp, body, http.StatusForbidden, CodeNoBindings)
		if !strings.Contains(body, idp.id+":nobody") || !strings.Contains(body, "ask an administrator") {
			t.Errorf("refusal message is not helpful: %q", body)
		}
		if status, _ := b.me(); status != http.StatusUnauthorized {
			t.Errorf("/me after refusal: %d", status)
		}
	})

	t.Run("200 IdP groups stay under the 4 KiB guard", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userMany)
		c := b.cookie(session.DefaultCookieName)
		size := len(c.Name) + 1 + len(c.Value)
		t.Logf("session cookie for a %d-group user: %d bytes (guard %d)", manyGroups, size, session.MaxCookieBytes)
		if size > session.MaxCookieBytes {
			t.Fatalf("cookie %d bytes", size)
		}
		if _, me := b.me(); strings.Join(me.Roles, ",") != "viewer" {
			t.Errorf("roles = %v", me.Roles)
		}
	})

	t.Run("tampered state is rejected", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		cb := b.authenticateAtIDP(b.startLogin("/"), userAdmin)
		q := cb.Query()
		q.Set("state", q.Get("state")+"x")
		cb.RawQuery = q.Encode()
		resp, body := b.finish(cb)
		wantLoginError(t, resp, body, http.StatusBadRequest, CodeStateMismatch)
	})

	t.Run("callback without the pre-auth cookie is rejected", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		cb := b.authenticateAtIDP(b.startLogin("/"), userAdmin)
		resp, body := h.newBrowser().finish(cb) // another browser: login CSRF / stolen code
		wantLoginError(t, resp, body, http.StatusBadRequest, CodeMissingPreAuth)
	})

	t.Run("replayed callback is rejected", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		cb := b.authenticateAtIDP(b.startLogin("/"), userAdmin)
		if resp, body := b.finish(cb); resp.StatusCode != http.StatusFound {
			t.Fatalf("first callback: %d %s", resp.StatusCode, body)
		}
		resp, body := b.finish(cb)
		wantLoginError(t, resp, body, http.StatusBadRequest, CodeMissingPreAuth)
	})

	t.Run("tampered nonce is rejected", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		authURL := b.startLogin("/")
		q := authURL.Query()
		q.Set("nonce", "attacker-chosen-nonce")
		authURL.RawQuery = q.Encode()
		resp, body := b.finish(b.authenticateAtIDP(authURL, userAdmin))
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeNonceMismatch)
	})

	t.Run("tampered PKCE challenge is rejected", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		authURL := b.startLogin("/")
		q := authURL.Query()
		q.Set("code_challenge", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM")
		authURL.RawQuery = q.Encode()
		resp, body := b.finish(b.authenticateAtIDP(authURL, userAdmin))
		wantLoginError(t, resp, body, http.StatusUnauthorized, CodeExchangeFailed)
	})

	t.Run("mutating request without X-Kudurru-CSRF is rejected", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userAdmin)
		_, me := b.me()
		cases := []struct {
			name string
			csrf string
			hdr  map[string]string
			want int
		}{
			{"no header", "", nil, http.StatusForbidden},
			{"wrong header", "not-the-token", nil, http.StatusForbidden},
			{"right header", me.CSRF, nil, http.StatusOK},
			{"right header, cross-site fetch", me.CSRF, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
			{"right header, foreign Origin", me.CSRF, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		}
		for _, c := range cases {
			if got := b.mutate(c.csrf, c.hdr); got != c.want {
				t.Errorf("%s: status %d, want %d", c.name, got, c.want)
			}
		}
	})

	t.Run("sessionsValidAfter in the future invalidates the session", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userAdmin)
		_, me := b.me()
		h.validAfter.Store(me.Principal.ID, h.clk.Now().Add(time.Hour))
		if status, _ := b.me(); status != http.StatusUnauthorized {
			t.Fatalf("/me after sessionsValidAfter: %d", status)
		}
		if b.cookie(session.DefaultCookieName) != nil {
			t.Error("revoked cookie still in the jar")
		}
		// Once sessionsValidAfter is in the past, a fresh login works.
		h.validAfter.Store(me.Principal.ID, h.clk.Now().Add(-time.Minute))
		b.mustLogin(userAdmin)
		if status, _ := b.me(); status != http.StatusOK {
			t.Fatalf("fresh login after revocation: %d", status)
		}
	})

	t.Run("idle timeout slides", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userAdmin)
		first := b.cookie(session.DefaultCookieName).Value
		h.clk.Advance(20 * time.Minute)
		if status, _ := b.me(); status != http.StatusOK {
			t.Fatalf("20m: %d", status)
		}
		if b.cookie(session.DefaultCookieName).Value == first {
			t.Fatal("20m: cookie was not re-sealed")
		}
		h.clk.Advance(20 * time.Minute) // 40m after login, 20m after last use
		if status, _ := b.me(); status != http.StatusOK {
			t.Fatalf("40m with activity at 20m: %d", status)
		}
		if status := h.meWithCookie(first); status != http.StatusUnauthorized {
			t.Fatalf("original cookie at 40m: %d, want 401 (idle_exp 30m)", status)
		}
		h.clk.Advance(31 * time.Minute)
		if status, _ := b.me(); status != http.StatusUnauthorized {
			t.Fatalf("31m idle: %d", status)
		}
	})

	t.Run("absolute lifetime does not slide", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userAdmin)
		elapsed := time.Duration(0)
		for elapsed+25*time.Minute < session.DefaultLifetime {
			h.clk.Advance(25 * time.Minute)
			elapsed += 25 * time.Minute
			if status, _ := b.me(); status != http.StatusOK {
				t.Fatalf("active user at %v: %d", elapsed, status)
			}
		}
		h.clk.Advance(session.DefaultLifetime - elapsed)
		if status, _ := b.me(); status != http.StatusUnauthorized {
			t.Fatalf("active user at 12h: %d, want 401", status)
		}
	})

	t.Run("previous key still opens old cookies during rotation", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userAdmin)
		oldCookie := b.cookie(session.DefaultCookieName).Value
		inflight := h.newBrowser()
		inflightAuth := inflight.startLogin("/zones") // pre-auth sealed under key A

		h.configure(idp, keyB, keyA) // rolling restart with KUDURRU_ENCRYPTION_KEY_PREVIOUS
		if status, _ := b.me(); status != http.StatusOK {
			t.Fatalf("old cookie during rotation: %d", status)
		}
		h.clk.Advance(2 * time.Minute)
		if status, _ := b.me(); status != http.StatusOK {
			t.Fatalf("touch during rotation: %d", status)
		}
		resealed := b.cookie(session.DefaultCookieName).Value
		if resealed == oldCookie {
			t.Fatal("cookie was not re-sealed under the new key")
		}
		if resp, body := inflight.finish(inflight.authenticateAtIDP(inflightAuth, userAdmin)); resp.StatusCode != http.StatusFound {
			t.Fatalf("login started before rotation: %d %s", resp.StatusCode, body)
		}

		h.configure(idp, keyB, nil) // previous key removed after one lifetime
		if status := h.meWithCookie(resealed); status != http.StatusOK {
			t.Fatalf("re-sealed cookie after rotation: %d", status)
		}
		if status := h.meWithCookie(oldCookie); status != http.StatusUnauthorized {
			t.Fatalf("old cookie after previous key removed: %d", status)
		}
	})

	t.Run("logout clears the cookie and needs CSRF", func(t *testing.T) {
		reset(t)
		b := h.newBrowser()
		b.mustLogin(userAdmin)
		_, me := b.me()
		if got := b.logout(""); got != http.StatusForbidden {
			t.Fatalf("logout without CSRF: %d", got)
		}
		if got := b.logout(me.CSRF); got != http.StatusNoContent {
			t.Fatalf("logout: %d", got)
		}
		if status, _ := b.me(); status != http.StatusUnauthorized {
			t.Fatalf("/me after logout: %d", status)
		}
	})

	t.Run("next is same-origin only", func(t *testing.T) {
		reset(t)
		for next, want := range map[string]string{
			"//evil.example/x":       "/",
			"https://evil.example/":  "/",
			"/zones/home?tab=record": "/zones/home?tab=record",
		} {
			b := h.newBrowser()
			resp, body := b.login(userAdmin, next)
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
				t.Errorf("next=%q: %d Location %q (%s)", next, resp.StatusCode, resp.Header.Get("Location"), body)
			}
		}
	})
}
