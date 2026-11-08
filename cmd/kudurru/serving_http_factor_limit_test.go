package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Factor proof admission now intentionally advances operational counters even
// when no authentication state changes. Assert every other payload and object
// revision remains exact, rather than weakening the original no-factor-effect
// checks to merely inspect an enabled flag.
func factorAdmissionOnly(before, after *replication.Image) bool {
	if before == nil || after == nil || after.Revision != before.Revision+1 || before.PayloadSHA256 == after.PayloadSHA256 || sqliteadapter.ValidateImage(after) != nil {
		return false
	}
	strip := func(rows []replication.Entry) ([]replication.Entry, []byte) {
		kept := make([]replication.Entry, 0, len(rows))
		var counter []byte
		for _, row := range rows {
			if row.Key == sqliteadapter.Key {
				counter = row.Data
			} else {
				kept = append(kept, row)
			}
		}
		return kept, counter
	}
	a, b := *before, *after
	var old, next []byte
	a.Payload.Runtime, old = strip(a.Payload.Runtime)
	b.Payload.Runtime, next = strip(b.Payload.Runtime)
	if len(next) == 0 || bytes.Equal(old, next) {
		return false
	}
	// These are the metadata fields changed by the intended operational commit.
	b.Revision, b.CreatedAt, b.Parent, b.PayloadSHA256 = a.Revision, a.CreatedAt, a.Parent, a.PayloadSHA256
	return reflect.DeepEqual(a, b)
}

func TestServingHTTPFactorsShareFailureHistoryAcrossAdapters(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	newAdapter := func() http.Handler {
		t.Helper()
		h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
		if err != nil {
			t.Fatal(err)
		}
		return h.Middleware(h.Handler())
	}
	servers := []http.Handler{f.h.Middleware(f.h.Handler()), newAdapter()}
	call := func(i int, path, body string, authenticated bool, want int) {
		t.Helper()
		localUsersFresh(t, f)
		r := httptest.NewRequest(http.MethodPost, "http://kudurru.test/api/v1"+path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.20:3131"
		r.Header.Set("Content-Type", "application/json")
		if authenticated {
			r.AddCookie(cookie)
			r.Header.Set(auth.CSRFHeader, info.CSRF)
		}
		w := httptest.NewRecorder()
		servers[i].ServeHTTP(w, r)
		expectServingHTTPStatus(t, w, want)
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("refused factor proof issued or changed a cookie")
		}
	}
	for i := range 5 {
		path := "/auth/totp/setup"
		if i%2 == 1 {
			path = "/auth/totp/disable"
		}
		before, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		call(i%2, path, `{"currentPassword":"wrong-password"}`, true, 401)
		after, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil || !factorAdmissionOnly(before, after) {
			t.Fatal("failed proof did not commit only shared counters", err)
		}
	}
	servers[1] = newAdapter()
	for i := range servers {
		call(i, "/auth/totp/setup", `{"currentPassword":"initial-password"}`, true, 429)
		call(i, "/auth/totp/confirm", `{"currentPassword":"initial-password","challenge":"unverified","code":"000000"}`, true, 429)
		call(i, "/auth/login", `{"username":"root-command","password":"initial-password"}`, false, 429)
	}
}
