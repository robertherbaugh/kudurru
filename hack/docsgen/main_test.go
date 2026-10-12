package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReferenceDrift fails when a committed reference page differs from
// what the generator writes today.
func TestReferenceDrift(t *testing.T) {
	root := filepath.Join("..", "..")
	pages, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		got, err := os.ReadFile(filepath.Join(root, p.Path)) //nolint:gosec // a generated page
		if err != nil {
			t.Errorf("%s: %v (run `make docs-gen`)", p.Path, err)
			continue
		}
		if string(got) != string(p.Body) {
			t.Errorf("%s is stale; run `make docs-gen` and commit the result", p.Path)
		}
	}
	stale, err := stalePages(root, pages)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		t.Errorf("%s documents a CRD that no longer exists; run `make docs-gen`", s)
	}
}

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"a <b> c":                   "a &lt;b&gt; c",
		"*.suffix and `x|y` | z":    `\*.suffix and ` + "`x\\|y`" + ` \| z`,
		"keep `<node.dataDir>` raw": "keep `<node.dataDir>` raw",
		"line\n  break":             "line break",
		"KUDURRU_X[_FILE]":          `KUDURRU\_X\[\_FILE\]`,
	} {
		if got := text(in); got != want {
			t.Errorf("text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlow(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{
		{"", `""`},
		{"2s", "2s"},
		{[]string{}, "[]"},
		{[]map[string]any{{"address": "0.0.0.0:53", "protocols": []string{"udp", "tcp"}}}, `[{address: "0.0.0.0:53", protocols: [udp, tcp]}]`},
		{map[string]int{"24": 0}, "{24: 0}"},
		{-1, "-1"},
	} {
		if got := flow(c.in); got != c.want {
			t.Errorf("flow(%#v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestEnvName(t *testing.T) {
	if got := envName("listeners.api.tls.certFile"); got != "KUDURRU_LISTENERS__API__TLS__CERTFILE" {
		t.Fatal(got)
	}
	if !strings.HasPrefix(envName("log.level"), "KUDURRU_LOG__") {
		t.Fatal(envName("log.level"))
	}
}
