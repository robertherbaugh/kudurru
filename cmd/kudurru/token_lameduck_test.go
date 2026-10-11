package main

import (
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

