package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// socketDir returns a short temp dir: unix socket paths are limited to
// about 104 bytes and t.TempDir() can be longer.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kdr-admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeAdminHTTP mirrors internal/auth/adminsock: HTTP/1.1 over the unix
// socket, strict JSON bodies, API-style errors.
type fakeAdminHTTP struct {
	path string
	mu   sync.Mutex
	reqs []string
}

func newFakeAdminHTTP(t *testing.T) *fakeAdminHTTP {
	t.Helper()
	s := &fakeAdminHTTP{path: filepath.Join(socketDir(t), "kudurru.sock")}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	strict := func(w http.ResponseWriter, r *http.Request, v any) bool {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path+" "+string(b))
		s.mu.Unlock()
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"key":"invalid","message":"body: `+strings.ReplaceAll(err.Error(), `"`, `'`)+`"}}`)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /admin/v1/reset-password", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Create   bool   `json:"create"`
		}
		if !strict(w, r, &req) {
			return
		}
		if req.Username == "ghost" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"key":"not_found","message":"no local user \"ghost\""},"took_ms":0.1}`)
			return
		}
		res := map[string]any{"username": firstNonEmpty(req.Username, "admin"), "mustChangePassword": true, "created": req.Create}
		if req.Password == "" {
			res["password"] = "Gen3rated-Pass"
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /admin/v1/create-token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			User        string `json:"user"`
			Description string `json:"description"`
			ExpiresIn   string `json:"expiresIn"`
		}
		if !strict(w, r, &req) {
			return
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":"abc123def456","token":"kdr_abc123def456_xyz","owner":{"kind":"ServiceAccount","name":"break-glass","namespace":"kudurru-system"},"expiresAt":"2026-10-12T12:00:00Z"}`)
	})
	mux.HandleFunc("GET /admin/v1/list-users", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"users":[{"name":"admin","builtinAdmin":true,"disabled":false,"mustChangePassword":true,"lastLoginAt":"2026-10-11T08:00:00Z"},{"name":"viewer","displayName":"View Only","builtinAdmin":false,"disabled":true,"mustChangePassword":false,"groups":["ops"]}]}`)
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // G112: test server on a unix socket
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

func TestAdminSocketHTTP(t *testing.T) {
	sock := newFakeAdminHTTP(t)
	h := newHarness(t)
	h.env["KUDURRU_ADMIN_SOCKET"] = sock.path

	out := h.mustRun("admin", "reset-password")
	if out != "Password of admin reset.\nNew password (shown once): Gen3rated-Pass\nThe user must change it at the next login.\n" {
		t.Fatalf("reset-password output %q", out)
	}
	h.stdin = "chosen-pass\n"
	out = h.mustRun("admin", "reset-password", "viewer", "--password-stdin", "--socket", sock.path)
	if strings.Contains(out, "shown once") {
		t.Fatalf("a chosen password must not be echoed: %q", out)
	}
	if out := h.mustRun("admin", "reset-password", "root2", "--create"); !strings.HasPrefix(out, "User root2 created.\n") {
		t.Fatalf("--create output %q", out)
	}
	out, errOut, err := h.run("admin", "create-token", "--description", "ci", "--expires-in", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if out != "kdr_abc123def456_xyz\n" || errOut != "token abc123def456 created (shown once) for ServiceAccount kudurru-system/break-glass, expires 2026-10-12T12:00:00Z\n" {
		t.Fatalf("create-token: %q %q", out, errOut)
	}
	golden(t, h, "admin-list-users", h.mustRun("admin", "list-users"))
	golden(t, h, "admin-list-users-wide", h.mustRun("admin", "list-users", "-o", "wide"))

	if _, _, err := h.run("admin", "reset-password", "ghost"); err == nil || err.Error() != `admin reset-password: not_found: no local user "ghost"` {
		t.Fatalf("refused op: %v", err)
	}
	if _, _, err := h.run("admin", "create-token", "--expires-in", "soon"); err == nil {
		t.Fatal("bad --expires-in accepted")
	}
	sock.mu.Lock()
	reqs := sock.reqs
	sock.mu.Unlock()
	want := []string{
		"POST /admin/v1/reset-password {}",
		`POST /admin/v1/reset-password {"password":"chosen-pass","username":"viewer"}`,
		`POST /admin/v1/reset-password {"create":true,"username":"root2"}`,
		`POST /admin/v1/create-token {"description":"ci","expiresIn":"1h"}`,
		"GET /admin/v1/list-users",
		"GET /admin/v1/list-users",
		`POST /admin/v1/reset-password {"username":"ghost"}`,
	}
	if strings.Join(reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(reqs, "\n"), strings.Join(want, "\n"))
	}

	h.env["KUDURRU_ADMIN_SOCKET"] = filepath.Join(socketDir(t), "none.sock")
	if _, _, err := h.run("admin", "list-users"); err == nil || !strings.Contains(err.Error(), "inside the kudurru container") {
		t.Fatalf("missing socket: %v", err)
	}
}

// TestAdminSocketNDJSONFallback runs against a server that speaks the
// line protocol {op,args} → {ok,result,error}: auto mode falls back to it.
func TestAdminSocketNDJSONFallback(t *testing.T) {
	path := filepath.Join(socketDir(t), "k.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var mu sync.Mutex
	var ops []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					Op string `json:"op"`
				}
				if json.Unmarshal(line, &req) != nil {
					_, _ = c.Write([]byte(`{"ok":false,"error":"bad request"}` + "\n"))
					return
				}
				mu.Lock()
				ops = append(ops, req.Op)
				mu.Unlock()
				_, _ = c.Write([]byte(`{"ok":true,"result":[{"name":"admin","builtinAdmin":true,"mustChangePassword":true}]}` + "\n"))
			}(c)
		}
	}()
	h := newHarness(t)
	out := h.mustRun("admin", "list-users", "--socket", path)
	if !strings.Contains(out, "admin   true    false") {
		t.Fatalf("list-users over ndjson:\n%s", out)
	}
	h.mustRun("admin", "list-users", "--socket", path, "--protocol", "ndjson")
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(ops, ",") != "list-users,list-users" {
		t.Fatalf("ops %v", ops)
	}
}
