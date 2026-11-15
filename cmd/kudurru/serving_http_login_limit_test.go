package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestServingHTTPSharedPasswordCountersAcrossAdapters(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	other, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	servers := []http.Handler{f.h.Middleware(f.h.Handler()), other.Middleware(other.Handler())}
	call := func(i int, password string, want int) {
		t.Helper()
		current, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool { return v.Base().Same(current.Generation()) })
		r := httptest.NewRequest(http.MethodPost, "http://kudurru.test/api/v1/auth/login", strings.NewReader(`{"username":"root-command","password":"`+password+`"}`))
		r.RemoteAddr = "192.0.2.21:1234"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		servers[i].ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("adapter %d returned %d, want %d: %s", i, w.Code, want, w.Body.String())
		}
		if want != 200 && len(w.Result().Cookies()) != 0 {
			t.Fatal("refused login issued cookie")
		}
	}
	for i := range 5 {
		call(i%2, "incorrect", 401)
	}
	call(0, "initial-password", 429)
	call(1, "initial-password", 429)
	recreated, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	servers[1] = recreated.Middleware(recreated.Handler())
	call(1, "initial-password", 429)
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteadapter.ValidateImage(image); err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, row := range image.Payload.Runtime {
		if row.Key == sqliteadapter.Key {
			rows++
		}
	}
	if rows != 1 {
		t.Fatal("missing shared runtime row")
	}
	// A separate physical follower captures its install before reading source.
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "follower.db")
	open := func() *sqlite.Store {
		t.Helper()
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "login-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	t.Cleanup(func() { _ = follower.Close() })
	if err := follower.Enroll(t.Context(), f.s.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx := captureServingReplica(t, follower)
	source, err := f.s.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(installCtx, *source)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: source.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	after, err := follower.ReadGeneration(t.Context())
	if err != nil || !after.Generation().Same(installed) {
		t.Fatal("follower restart changed installed source", err)
	}
	find := func(image *replication.Image) []byte {
		for _, row := range image.Payload.Runtime {
			if row.Key == sqliteadapter.Key {
				return row.Data
			}
		}
		return nil
	}
	if len(find(source)) == 0 || !bytes.Equal(find(source), find(after)) {
		t.Fatal("follower lost lockout document")
	}
	entered := false
	err = follower.Command(t.Context(), follower.Authority().Term, func(context.Context, *sqlite.CommandTx) error { entered = true; return nil })
	if err == nil || entered {
		t.Fatal("follower granted local login mutation", err)
	}
}
