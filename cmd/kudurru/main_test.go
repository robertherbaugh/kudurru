package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) string {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestVersionAndSchema(t *testing.T) {
	if out := run(t, "version"); !strings.HasPrefix(out, "kudurru ") {
		t.Fatalf("unexpected version output: %q", out)
	}
	out := run(t, "config", "schema")
	for _, key := range []string{"node.name", "listeners.dns", "upstreams.groups"} {
		if !strings.Contains(out, key) {
			t.Errorf("schema output missing %s", key)
		}
	}
}

func TestConfigShowUsesDefaultsWhenDefaultFileMissing(t *testing.T) {
	t.Setenv("KUDURRU_CONFIG", "")
	prev := defaultConfigPath
	defaultConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	t.Cleanup(func() { defaultConfigPath = prev })
	out := run(t, "config", "show")
	if !strings.Contains(out, "dataDir") {
		t.Fatalf("expected effective config, got %q", out)
	}
}

func TestExplicitMissingConfigIsAnError(t *testing.T) {
	root := newRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml"), "config", "show"})
	if err := root.Execute(); err == nil {
		t.Fatal("an explicitly given missing config file must be an error")
	}
}
