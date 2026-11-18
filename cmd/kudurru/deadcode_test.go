package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	out := filepath.Join(t.TempDir(), "kudurru")
	var dump []byte
	cmd := exec.Command(gobin, "build", "-ldflags=-dumpdep", "-o", out, ".") //nolint:gosec // the test's own toolchain
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	dump, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, tail(dump))
	}
	for _, sym := range []string{"text/template.(*state).evalField", "text/template.(*state).evalCall"} {
		if dumpMentions(dump, sym) {
			t.Errorf("%s is reachable: something executes a template; render by hand (see internal/blockpage/page.go)", sym)
		}
	}
}

// dumpMentions reports whether a -dumpdep line ("from -> to") names sym
// exactly. A substring match is wrong: the linker shares argument-info
// data symbols between functions, and one can be named after a different
// function that merely starts with sym (for example
// "text/template.(*state).evalFieldNode.arginfo1" is not evalField).
func dumpMentions(dump []byte, sym string) bool {
	for _, line := range bytes.Split(dump, []byte("\n")) {
		from, to, ok := bytes.Cut(line, []byte(" -> "))
		if !ok {
			continue
		}
		if string(from) == sym || string(to) == sym {
			return true
		}
	}
	return false
}

func TestDumpMentionsMatchesWholeSymbols(t *testing.T) {
	const sym = "text/template.(*state).evalField"
	for _, tc := range []struct {
		name, dump string
		want       bool
	}{
		{"callee", "pkg.f -> " + sym + "\n", true},
		{"caller", sym + " -> reflect.Value.MethodByName\n", true},
		{"data symbol of a longer name", "pkg.g -> text/template.(*state).evalFieldNode.arginfo1\n", false},
		{"longer function", "pkg.g -> text/template.(*state).evalFieldChain\n", false},
		{"unrelated", "pkg.f -> pkg.g\n", false},
	} {
		if got := dumpMentions([]byte(tc.dump), sym); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func tail(b []byte) []byte {
	if len(b) > 4096 {
		return b[len(b)-4096:]
	}
	return b
}
