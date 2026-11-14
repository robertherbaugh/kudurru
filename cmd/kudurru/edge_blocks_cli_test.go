package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEdgeBlockCLI drives kudurru edge block, blocks and unblock against a
// fake API: the request shapes, structured output of every verb, and an
// unblock argument that is not a block ID refused before any request.
func TestEdgeBlockCLI(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	until := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	block := map[string]any{"id": "00000000000000a1", "prefix": "192.0.2.0/24", "createdAt": until.Add(-time.Hour), "until": until}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery+" "+strings.TrimSpace(string(body)))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/edge/blocks":
			_ = json.NewEncoder(w).Encode(block)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/edge/blocks":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{block}, "revision": "1", "maxBlocks": 256})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/edge/blocks/00000000000000a1":
			_ = json.NewEncoder(w).Encode(block)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer server.Close()
	run := func(args ...string) (string, error) {
		o := newCLIOpts()
		o.getenv = func(string) string { return "" }
		root := newRootWith(o)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--cli-config", filepath.Join(t.TempDir(), "contexts.yaml"), "--server", server.URL, "--token", "kdr_test"}, args...))
		err := root.ExecuteContext(t.Context())
		return out.String(), err
	}
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}

	out, err := run("-o", "json", "edge", "block", "192.0.2.0/24", "15m", "--reason", "query flood")
	if err != nil || !strings.Contains(out, `"id": "00000000000000a1"`) {
		t.Fatalf("block: %v\n%s", err, out)
	}
	if got := requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /api/v1/edge/blocks? ") ||
		!strings.Contains(got[0], `"seconds":900`) || !strings.Contains(got[0], `"prefix":"192.0.2.0/24"`) || !strings.Contains(got[0], `"reason":"query flood"`) {
		t.Fatalf("block request %q", got)
	}
	if out, err := run("edge", "blocks"); err != nil || !strings.Contains(out, "192.0.2.0/24") {
		t.Fatalf("blocks: %v\n%s", err, out)
	}
	out, err = run("-o", "json", "edge", "unblock", "00000000000000a1")
	if err != nil || !strings.Contains(out, `"prefix": "192.0.2.0/24"`) {
		t.Fatalf("unblock structured output: %v\n%s", err, out)
	}
	if out, err := run("edge", "unblock", "00000000000000a1"); err != nil || !strings.Contains(out, "edge block 00000000000000a1 revoked") {
		t.Fatalf("unblock: %v\n%s", err, out)
	}
	before := len(requests())
	for _, arg := range []string{"../zones/x", "00000000000000a1?x=y", "00000000000000A1", "a1", "00000000000000a1/"} {
		if _, err := run("edge", "unblock", arg); err == nil {
			t.Fatalf("unblock %q accepted", arg)
		}
	}
	if got := requests(); len(got) != before {
		t.Fatalf("an invalid unblock argument reached the API: %q", got[before:])
	}
}
