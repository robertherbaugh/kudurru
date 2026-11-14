package stack

import (
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
)

func TestExternalAccountsUnsupportedStackRefusesAfterPermission(t *testing.T) {
	e := newEnv(t, authtest.Memory(t), testConfig)
	admin, csrf := e.login("admin", "admin-password-1")
	for _, route := range []struct{ method, path string }{
		{"GET", "/iam/external-users"}, {"GET", "/iam/external-users/" + strings.Repeat("a", 64)},
		{"POST", "/iam/external-users/" + strings.Repeat("a", 64) + "/disable"},
		{"POST", "/iam/external-users/" + strings.Repeat("a", 64) + "/enable"},
		{"POST", "/iam/external-users/" + strings.Repeat("a", 64) + "/logout"},
	} {
		denied := e.do(route.method, "/api/v1"+route.path, nil)
		if denied.code != http.StatusUnauthorized {
			t.Fatal("anonymous route admitted", denied.code)
		}
		got := e.do(route.method, "/api/v1"+route.path, nil, withCookie(admin), withHeader(auth.CSRFHeader, csrf))
		if got.code != http.StatusServiceUnavailable || got.errKey() != "external_users_unavailable" {
			t.Fatal("unsupported runtime implied account capability", got.code)
		}
	}
}
