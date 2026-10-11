package stack

import (
	"net/http"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestRecreatedUserDoesNotInheritCredentials is the regression test of the
// M5 security finding "a re-created local account inherits the sessions and
// API tokens of a deleted account with the same name": principals of local
// users are keyed by name ("local:<name>"), so a token minted by a deleted
// "dave" acted as a new "dave", including a builtin admin created with
// `kudurru admin reset-password --create`.
func TestRecreatedUserDoesNotInheritCredentials(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		e := newEnv(t, st, testConfig)
		if _, err := e.s.Users().Create(e.ctx, users.CreateOptions{Name: "dave", Password: "dave-password-1", Groups: []string{"staff"}}); err != nil {
			t.Fatal(err)
		}
		cookie, csrf := e.login("dave", "dave-password-1")
		r := e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{Description: "ci"}, withCookie(cookie), withHeader(auth.CSRFHeader, csrf))
		if r.code != http.StatusCreated {
			t.Fatalf("create token: %d %s", r.code, r.raw)
		}
		tok, _ := r.body["token"].(string)
		for _, o := range []reqOpt{bearer(tok), withCookie(cookie)} {
			if r := e.do(http.MethodGet, "/api/v1/zones", nil, o); r.code != 200 {
				t.Fatalf("before deletion: %d %s", r.code, r.raw)
			}
		}

		// The account is deleted (store-level, as v0.1 has no user delete
		// route) and, later, an account with the same name is created as a
		// builtin admin.
		key := store.Key{Kind: internalv1.KindLocalUser, Name: "dave"}
		if err := st.Delete(e.ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the deletion to reach the user cache", func() bool { _, ok := e.s.Users().Get("dave"); return !ok })
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer(tok)); r.code != 401 {
			t.Fatalf("token of a deleted user: %d %s", r.code, r.raw)
		}
		time.Sleep(1100 * time.Millisecond) // creation timestamps have one-second resolution
		if _, err := e.s.Users().Create(e.ctx, users.CreateOptions{Name: "dave", Password: "new-dave-password", BuiltinAdmin: true}); err != nil {
			t.Fatal(err)
		}
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer(tok)); r.code != 401 {
			t.Fatalf("old token on the re-created account: %d %s (path %v)", r.code, r.raw, r.body["path"])
		}
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(cookie)); r.code != 401 {
			t.Fatalf("old session on the re-created account: %d %s", r.code, r.raw)
		}

		// The new account's own credentials work.
		cookie2, csrf2 := e.login("dave", "new-dave-password")
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(cookie2)); r.code != 200 || r.body["path"] != "local" {
			t.Fatalf("new session: %d %s", r.code, r.raw)
		}
		r = e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{Description: "new"}, withCookie(cookie2), withHeader(auth.CSRFHeader, csrf2))
		if r.code != http.StatusCreated {
			t.Fatalf("create token: %d %s", r.code, r.raw)
		}
		tok2, _ := r.body["token"].(string)
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer(tok2)); r.code != 200 {
			t.Fatalf("new token: %d %s", r.code, r.raw)
		}
	})
}
