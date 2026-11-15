package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
)

func TestServingHTTPPasswordChangesShareRejectedProofs(t *testing.T) {
	for _, mode := range []string{"password", "factor"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			cookie, info := f.login(t)
			var seed *totp.Seed
			if mode == "factor" {
				seed = factorInstall(t, f)
			}
			newAdapter := func() http.Handler {
				h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
				if err != nil {
					t.Fatal(err)
				}
				return h.Middleware(h.Handler())
			}
			servers := []http.Handler{f.h.Middleware(f.h.Handler()), newAdapter()}
			call := func(i int, path, body string, want int) {
				t.Helper()
				localUsersFresh(t, f)
				r := httptest.NewRequest(http.MethodPost, "http://kudurru.test/api/v1"+path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:3131"
				r.Header.Set("Content-Type", "application/json")
				r.AddCookie(cookie)
				r.Header.Set(auth.CSRFHeader, info.CSRF)
				w := httptest.NewRecorder()
				servers[i].ServeHTTP(w, r)
				expectServingHTTPStatus(t, w, want)
				if len(w.Result().Cookies()) != 0 {
					t.Fatal("failed password change emitted a cookie")
				}
			}
			for i := range 5 {
				proof := api.PasswordChangeRequest{CurrentPassword: "wrong-password", NewPassword: "replacement-password", RevokeTokens: true}
				if seed != nil {
					proof.CurrentPassword = "initial-password"
					// Use a syntactically valid numeric proof that is actually wrong
					// in the surrounding windows, not a decoder-only rejection.
					for _, code := range []string{"000000", "000001", "000002", "000003", "000004", "000005"} {
						valid := false
						for _, offset := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
							_, err := totp.Match(seed, code, f.h.now().Add(offset))
							valid = valid || err == nil
						}
						if !valid {
							proof.TOTPCode = code
							break
						}
					}
					if proof.TOTPCode == "" {
						t.Fatal("no invalid numeric factor candidate")
					}
				}
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				call(i%2, "/auth/password", factorBody(t, proof), 401)
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !factorAdmissionOnly(before, after) {
					t.Fatal("invalid proof changed password, factor, tokens or sessions", err)
				}
				if seed != nil {
					// Keep the production local one-second throttle intact. These
					// are separate admitted requests, never retries of a refusal.
					select {
					case <-time.After(1100 * time.Millisecond):
					case <-t.Context().Done():
						t.Fatal(t.Context().Err())
					}
				}
			}
			servers[1] = newAdapter()
			call(1, "/auth/password", factorBody(t, api.PasswordChangeRequest{CurrentPassword: "initial-password", NewPassword: "replacement-password"}), 429)
			call(1, "/auth/login", `{"username":"root-command","password":"initial-password"}`, 429)
		})
	}
}
