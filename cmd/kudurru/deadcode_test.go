package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNoTemplateExecution keeps template execution out of the binary.
// Executing a text/template or html/template calls
// reflect.Value.MethodByName with a dynamic name, which turns off the
// linker's dead-method elimination: every exported method of every
// reachable type is kept (about 12 MB with client-go, D75). Packages may
// import text/template (cobra, cel-go do) as long as nothing reachable
// executes one; the block page renders by hand (internal/blockpage).
func TestNoTemplateExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("links the binary")
	}
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gobin); err != nil {
		gobin = "go"
	}
	out := filepath.Join(t.TempDir(), "kudurru")
	cmd := exec.Command(gobin, "build", "-ldflags=-dumpdep", "-o", out, ".") //nolint:gosec // the test's own toolchain
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	dump, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, tail(dump))
	}
	for _, sym := range [][]byte{[]byte("text/template.(*state).evalField"), []byte("text/template.(*state).evalCall")} {
		if bytes.Contains(dump, sym) {
			t.Errorf("%s is reachable: something executes a template; render by hand (see internal/blockpage/page.go)", sym)
		}
	}
}

func tail(b []byte) []byte {
	if len(b) > 4096 {
		return b[len(b)-4096:]
	}
	return b
}
