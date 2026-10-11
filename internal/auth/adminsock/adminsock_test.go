package adminsock

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
)

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ks") // short: sun_path is 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "kudurru.sock")
}

func client(path string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

type fixture struct {
	users  *users.Users
	tokens *tokens.Tokens
	path   string
	c      *http.Client
}

func start(t *testing.T, pol auth.PeerPolicy, api http.Handler) fixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED is Linux-only")
	}
	st := authtest.Memory(t)
	u, err := users.New(users.Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := tokens.New(tokens.Options{Store: st, MaxLifetime: 12 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	for _, prime := range []func(context.Context) error{u.Prime, tk.Prime} {
		if err := prime(ctx); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(Options{Policy: pol, Users: u, Tokens: tk, MaxTokenLifetime: 12 * time.Hour, API: api})
	if err != nil {
		t.Fatal(err)
	}
	path := socketPath(t)
	done := make(chan error, 1)
	ln, err := Listen(path, pol.GID)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return fixture{users: u, tokens: tk, path: path, c: client(path)}
}

func (f fixture) call(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, "http://kudurru"+path, &buf)
	resp, err := f.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestProtocol(t *testing.T) {
	var apiPrincipal auth.Principal
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiPrincipal, _ = auth.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})
	f := start(t, auth.PeerPolicy{GID: -1}, api)
	ctx := context.Background()

	fi, err := os.Stat(f.path)
	if err != nil || fi.Mode().Perm() != 0o660 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode %v, %v", fi.Mode(), err)
	}

	var list ListUsersResponse
	if code := f.call(t, http.MethodGet, "/admin/v1/list-users", nil, &list); code != 200 || len(list.Users) != 0 {
		t.Fatalf("empty list-users: %d %+v", code, list)
	}

	// reset-password of a missing admin: 404 unless create.
	var errBody struct {
		Error struct{ Key string } `json:"error"`
	}
	if code := f.call(t, http.MethodPost, "/admin/v1/reset-password", ResetPasswordRequest{}, &errBody); code != 404 || errBody.Error.Key != "not_found" {
		t.Fatalf("reset missing: %d %+v", code, errBody)
	}
	var reset ResetPasswordResponse
	if code := f.call(t, http.MethodPost, "/admin/v1/reset-password", ResetPasswordRequest{Create: true}, &reset); code != 200 || !reset.Created || reset.Password == "" || reset.Username != "admin" {
		t.Fatalf("reset create: %d %+v", code, reset)
	}
	if _, err := f.users.Login(ctx, "admin", reset.Password, "127.0.0.1"); err != nil {
		t.Fatalf("login with the reset password: %v", err)
	}
	reset = ResetPasswordResponse{}
	if code := f.call(t, http.MethodPost, "/admin/v1/reset-password", ResetPasswordRequest{Username: "admin", Password: "chosen-password"}, &reset); code != 200 || reset.Password != "" || !reset.MustChangePassword {
		t.Fatalf("reset chosen: %d %+v", code, reset)
	}
	if code := f.call(t, http.MethodPost, "/admin/v1/reset-password", map[string]any{"username": "admin", "pasword": "typo"}, &errBody); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}

	if code := f.call(t, http.MethodGet, "/admin/v1/list-users", nil, &list); code != 200 || len(list.Users) != 1 || !list.Users[0].BuiltinAdmin || !list.Users[0].MustChangePassword {
		t.Fatalf("list-users: %d %+v", code, list)
	}

	// create-token: break-glass service-account token, capped by maxLifetime.
	var tok CreateTokenResponse
	if code := f.call(t, http.MethodPost, "/admin/v1/create-token", CreateTokenRequest{}, &tok); code != 201 || tok.Token == "" || tok.ExpiresAt == nil {
		t.Fatalf("create-token: %d %+v", code, tok)
	}
	if left := time.Until(*tok.ExpiresAt); left > 12*time.Hour || left < 11*time.Hour {
		t.Fatalf("break-glass token lifetime %s, want maxLifetime 12h", left)
	}
	p, err := f.tokens.Verify(ctx, tok.Token, "")
	if err != nil || !p.BuiltinAdmin || p.ServiceAccount != tokens.DefaultNamespace+"/"+BreakGlassAccount {
		t.Fatalf("break-glass principal %+v, %v", p, err)
	}
	if code := f.call(t, http.MethodPost, "/admin/v1/create-token", CreateTokenRequest{User: "admin", ExpiresIn: "1h"}, &tok); code != 201 || tok.Owner.ID != "local:admin" {
		t.Fatalf("personal token: %d %+v", code, tok)
	}
	if code := f.call(t, http.MethodPost, "/admin/v1/create-token", CreateTokenRequest{User: "admin", ExpiresIn: "48h"}, &errBody); code != 400 {
		t.Fatalf("over maxLifetime: %d", code)
	}

	// Other paths reach the API as the local root.
	if code := f.call(t, http.MethodGet, "/api/v1/zones", nil, nil); code != http.StatusTeapot {
		t.Fatalf("API passthrough: %d", code)
	}
	if apiPrincipal.Kind != auth.KindLocalRoot || !apiPrincipal.BuiltinAdmin || apiPrincipal.ID != "unix:"+itoa(os.Getuid()) {
		t.Fatalf("API principal %+v", apiPrincipal)
	}
}

func TestPeerRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("uid 0 is always admitted")
	}
	// A policy for another server uid and no group: this process is a
	// foreign peer.
	f := start(t, auth.PeerPolicy{GID: -1, ServerUID: os.Getuid() + 1}, nil)
	var errBody struct {
		Error struct{ Key string } `json:"error"`
	}
	if code := f.call(t, http.MethodGet, "/admin/v1/list-users", nil, &errBody); code != 403 || errBody.Error.Key != "forbidden" {
		t.Fatalf("foreign peer: %d %+v", code, errBody)
	}
	// The configured gid admits it.
	g := start(t, auth.PeerPolicy{GID: os.Getgid(), ServerUID: os.Getuid() + 1}, nil)
	if code := g.call(t, http.MethodGet, "/admin/v1/list-users", nil, nil); code != 200 {
		t.Fatalf("gid peer: %d", code)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("unix sockets")
	}
	path := socketPath(t)
	ln, err := Listen(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, -1); err == nil {
		t.Fatal("a second Listen on a live socket succeeded")
	}
	// A stale socket file (a crash leaves it behind) is replaced.
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}
	ln2, err := Listen(path, -1)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	_ = ln2.Close()
	if err := os.WriteFile(path+".x", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path+".x", -1); err == nil {
		t.Fatal("Listen replaced a regular file")
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
