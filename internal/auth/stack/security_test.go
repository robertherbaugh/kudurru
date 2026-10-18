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

// TestZoneAdminManagesOwnTokens is the regression test of the M5 security
// follow-up "principals whose only binding is zone-scoped get 403 on
// /auth/tokens": Guard checked tokens.own at cluster scope, which a
// zone-admin@zone:home binding cannot satisfy. tokens.own now counts at
// any scope the principal holds, and token scopes still only restrict.
func TestZoneAdminManagesOwnTokens(t *testing.T) {
	const cfg = `
auth:
  session: { secure: never }
rbac:
  bindings:
  - { role: zone-admin, scope: { kind: zone, name: home }, subjects: [{ kind: User, name: zed }] }
`
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		e := newEnv(t, st, cfg)
		for _, u := range []string{"zed", "nobody"} {
			if _, err := e.s.Users().Create(e.ctx, users.CreateOptions{Name: u, Password: u + "-password-1"}); err != nil {
				t.Fatal(err)
			}
		}
		zed, zcsrf := e.login("zed", "zed-password-1")
		cookie := []reqOpt{withCookie(zed), withHeader(auth.CSRFHeader, zcsrf)}
		create := func(req CreateTokenRequest, opts ...reqOpt) resp {
			t.Helper()
			return e.do(http.MethodPost, "/api/v1/auth/tokens", req, opts...)
		}
		items := func(r resp) int { l, _ := r.body["items"].([]any); return len(l) }

		// Create, list, read and delete own tokens with a cookie session.
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens", nil, cookie...); r.code != 200 || items(r) != 0 {
			t.Fatalf("zone-admin listing own tokens: %d %s", r.code, r.raw)
		}
		plain := create(CreateTokenRequest{Description: "plain"}, cookie...)
		scoped := create(CreateTokenRequest{Description: "zone", Scopes: []string{"zone-admin@zone:home"}}, cookie...)
		wide := create(CreateTokenRequest{Description: "wide", Scopes: []string{"admin"}}, cookie...)
		for _, r := range []resp{plain, scoped, wide} {
			if r.code != http.StatusCreated {
				t.Fatalf("zone-admin creating a token: %d %s", r.code, r.raw)
			}
		}
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens", nil, cookie...); r.code != 200 || items(r) != 3 {
			t.Fatalf("zone-admin listing 3 own tokens: %d %s", r.code, r.raw)
		}
		plainID, _ := plain.body["id"].(string)
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens/"+plainID, nil, cookie...); r.code != 200 {
			t.Fatalf("zone-admin reading the own token: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodDelete, "/api/v1/auth/tokens/"+plainID, nil, cookie...); r.code != http.StatusNoContent {
			t.Fatalf("zone-admin deleting the own token: %d %s", r.code, r.raw)
		}
		// Other owners' tokens stay out of reach.
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens?all=true", nil, cookie...); r.code != 403 {
			t.Fatalf("zone-admin listing all tokens: %d %s", r.code, r.raw)
		}
		admin, acsrf := e.login("admin", "admin-password-1")
		adm := create(CreateTokenRequest{Description: "admin's"}, withCookie(admin), withHeader(auth.CSRFHeader, acsrf))
		admID, _ := adm.body["id"].(string)
		if r := e.do(http.MethodDelete, "/api/v1/auth/tokens/"+admID, nil, cookie...); r.code != 404 {
			t.Fatalf("zone-admin deleting the admin's token: %d %s", r.code, r.raw)
		}

		// The zone-scoped token works in its zone, manages tokens, and
		// cannot escalate: no cluster access, no broader child token.
		zt, _ := scoped.body["token"].(string)
		if r := e.do(http.MethodPost, "/api/v1/zones/home/records", nil, bearer(zt)); r.code != 200 {
			t.Fatalf("zone token writing its zone: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer(zt)); r.code != 403 {
			t.Fatalf("zone token at cluster scope: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens", nil, bearer(zt)); r.code != 200 || items(r) != 2 {
			t.Fatalf("zone token listing own tokens: %d %s", r.code, r.raw)
		}
		for _, sc := range [][]string{{"admin"}, {"zone-admin@zone:other"}, {"viewer"}} {
			if r := create(CreateTokenRequest{Scopes: sc}, bearer(zt)); r.code != 403 {
				t.Fatalf("zone token minting %v: %d %s", sc, r.code, r.raw)
			}
		}
		child := create(CreateTokenRequest{Description: "child"}, bearer(zt))
		if sc, _ := child.body["scopes"].([]any); child.code != http.StatusCreated || len(sc) != 1 || sc[0] != "zone-admin@zone:home" {
			t.Fatalf("zone token minting a child: %d %s", child.code, child.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens?all=true", nil, bearer(zt)); r.code != 403 {
			t.Fatalf("zone token listing all tokens: %d %s", r.code, r.raw)
		}

		// An admin scope on a zone-admin's token restricts nothing and adds
		// nothing: still no cluster access.
		wt, _ := wide.body["token"].(string)
		for _, path := range []string{"/api/v1/zones", "/api/v1/iam/users"} {
			if r := e.do(http.MethodGet, path, nil, bearer(wt)); r.code != 403 {
				t.Fatalf("admin-scoped token of a zone-admin on %s: %d %s", path, r.code, r.raw)
			}
		}
		if r := e.do(http.MethodPost, "/api/v1/zones/home/records", nil, bearer(wt)); r.code != 200 {
			t.Fatalf("admin-scoped token of a zone-admin in its zone: %d %s", r.code, r.raw)
		}

		// The admin's zone-scoped token manages tokens too, but only its
		// owner's, and only within its scope.
		azr := create(CreateTokenRequest{Scopes: []string{"zone-admin@zone:home"}}, withCookie(admin), withHeader(auth.CSRFHeader, acsrf))
		azt, _ := azr.body["token"].(string)
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens", nil, bearer(azt)); r.code != 200 {
			t.Fatalf("admin's zone token listing own tokens: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens?all=true", nil, bearer(azt)); r.code != 403 {
			t.Fatalf("admin's zone token listing all tokens: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodDelete, "/api/v1/auth/tokens/"+child.body["id"].(string), nil, bearer(azt)); r.code != 404 {
			t.Fatalf("admin's zone token deleting zed's token: %d %s", r.code, r.raw)
		}

		// A principal without any grant stays out.
		nobody, ncsrf := e.login("nobody", "nobody-password-1")
		if r := e.do(http.MethodGet, "/api/v1/auth/tokens", nil, withCookie(nobody)); r.code != 403 || r.errKey() != "forbidden" {
			t.Fatalf("grantless user listing tokens: %d %s", r.code, r.raw)
		}
		if r := create(CreateTokenRequest{}, withCookie(nobody), withHeader(auth.CSRFHeader, ncsrf)); r.code != 403 {
			t.Fatalf("grantless user creating a token: %d %s", r.code, r.raw)
		}
	})
}
