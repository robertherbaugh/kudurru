package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeAdminSocket serves the newline-delimited JSON admin protocol.
type fakeAdminSocket struct {
	path string
	mu   sync.Mutex
	reqs []map[string]any
}

func newFakeAdminSocket(t *testing.T) *fakeAdminSocket {
	t.Helper()
	dir, err := os.MkdirTemp("", "kdr-admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &fakeAdminSocket{path: filepath.Join(dir, "kudurru.sock")}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeAdminSocket) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Op   string         `json:"op"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		_, _ = c.Write([]byte(`{"ok":false,"error":"bad request"}` + "\n"))
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, map[string]any{"op": req.Op, "args": req.Args})
	s.mu.Unlock()
	var resp any
	switch req.Op {
	case adminOpResetPassword:
		if req.Args["username"] == "ghost" {
			resp = map[string]any{"ok": false, "error": map[string]any{"key": "not_found", "message": "no user ghost"}}
			break
		}
		res := map[string]any{"username": req.Args["username"], "mustChangePassword": true}
		if req.Args["password"] == nil {
			res["password"] = "Gen3rated-Pass"
		}
		resp = map[string]any{"ok": true, "result": res}
	case adminOpCreateToken:
		resp = map[string]any{"ok": true, "result": map[string]any{"id": "abc123def456", "token": "kdr_abc123def456_xyz", "owner": "admin"}}
	case adminOpListUsers:
		resp = map[string]any{"ok": true, "result": []map[string]any{
			{"username": "admin", "mustChangePassword": true, "roles": []string{"admin"}},
			{"username": "viewer", "disabled": true},
		}}
	default:
		resp = map[string]any{"ok": false, "error": "unknown op " + req.Op}
	}
	b, _ := json.Marshal(resp)
	_, _ = c.Write(append(b, '\n'))
}

func TestAdminSocket(t *testing.T) {
	sock := newFakeAdminSocket(t)
	h := newHarness(t)
	h.env["KUDURRU_ADMIN_SOCKET"] = sock.path

	out := h.mustRun("admin", "reset-password", "admin")
	if out != "Password of admin reset.\nNew password (shown once): Gen3rated-Pass\nThe user must change it at the next login.\n" {
		t.Fatalf("reset-password output %q", out)
	}
	h.stdin = "chosen-pass\n"
	out = h.mustRun("admin", "reset-password", "viewer", "--password-stdin", "--socket", sock.path)
	if strings.Contains(out, "shown once") {
		t.Fatalf("a chosen password must not be echoed: %q", out)
	}
	_, errOut, err := h.run("admin", "create-token", "--name", "ci", "--service-account", "ci", "--expires-in", "720h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "token abc123def456 created (shown once)") {
		t.Fatalf("create-token stderr %q", errOut)
	}
	golden(t, h, "admin-list-users", h.mustRun("admin", "list-users"))

	if _, _, err := h.run("admin", "reset-password", "ghost"); err == nil || err.Error() != "admin reset-password: not_found: no user ghost" {
		t.Fatalf("refused op: %v", err)
	}
	sock.mu.Lock()
	reqs := sock.reqs
	sock.mu.Unlock()
	if len(reqs) != 5 {
		t.Fatalf("requests %v", reqs)
	}
	if args := reqs[1]["args"].(map[string]any); args["password"] != "chosen-pass" {
		t.Fatalf("reset args %v", args)
	}
	if args := reqs[2]["args"].(map[string]any); args["serviceAccount"] != "ci" || args["expiresIn"] != "720h" || args["name"] != "ci" {
		t.Fatalf("create-token args %v", args)
	}

	h.env["KUDURRU_ADMIN_SOCKET"] = filepath.Join(t.TempDir(), "none.sock")
	if _, _, err := h.run("admin", "list-users"); err == nil || !strings.Contains(err.Error(), "inside the kudurru container") {
		t.Fatalf("missing socket: %v", err)
	}
}
