package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
)

func TestTokenGenerate(t *testing.T) {
	out := strings.TrimSpace(run(t, "token", "generate"))
	if _, ok := tokens.Parse(out); !ok {
		t.Fatalf("token generate printed %q, not a wire token", out)
	}
	if again := strings.TrimSpace(run(t, "token", "generate")); again == out {
		t.Error("two runs printed the same token")
	}
	file := filepath.Join(t.TempDir(), "bootstrap-token")
	run(t, "token", "generate", "--out", file)
	b, err := os.ReadFile(file) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Parse(strings.TrimSpace(string(b))); !ok {
		t.Errorf("--out wrote %q", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("--out mode %04o, want 0600", st.Mode().Perm())
	}
	if _, err := runErr("token", "generate", "--out", file); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("--out over an existing file: %v", err)
	}
}

func TestLameduck(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []lameduckRequest
		code = http.StatusOK
	)
	ops := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/lameduck" {
			http.NotFound(w, r)
			return
		}
		var req lameduckRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req)
		c := code
		mu.Unlock()
		w.WriteHeader(c)
	}))
	defer ops.Close()
	addr := strings.TrimPrefix(ops.URL, "http://")

	start := time.Now()
	out, err := runErr("lameduck", "300ms", "--ops-address", addr)
	if err != nil || !strings.Contains(out, "draining for 300ms") || !strings.Contains(out, "drained") {
		t.Fatalf("lameduck 300ms: %v %q", err, out)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Error("lameduck returned before the duration")
	}
	if out, err := runErr("lameduck", "7", "--no-wait", "--ops-address", addr); err != nil || strings.Contains(out, "drained") {
		t.Errorf("lameduck 7 --no-wait: %v %q", err, out)
	}
	mu.Lock()
	if len(got) != 2 || got[0].Seconds != 1 || got[1].Seconds != 7 {
		t.Errorf("requests %+v, want seconds 1 (300ms rounded up) and 7", got)
	}
	code = http.StatusServiceUnavailable
	mu.Unlock()
	if _, err := runErr("lameduck", "--no-wait", "--ops-address", addr); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
	if _, err := runErr("lameduck", "soon", "--ops-address", addr); err == nil {
		t.Error("bad duration accepted")
	}
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if _, err := runErr("lameduck", "--no-wait", "--ops-address", strings.TrimPrefix(old.URL, "http://")); err == nil ||
		!strings.Contains(err.Error(), "predates the lameduck endpoint") {
		t.Errorf("server without the endpoint: %v", err)
	}
}
