package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
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

func TestErrorHint(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&apiclient.Error{StatusCode: 401, Key: "unauthenticated"}, "kudurru login"},
		{&apiclient.Error{StatusCode: 403, Key: "forbidden"}, "RoleBinding"},
		{fmt.Errorf("wrapped: %w", &apiclient.Error{StatusCode: 503, Key: "read_only"}), "ha-static"},
		{&apiclient.Error{StatusCode: 404, Key: "not_found"}, ""},
		{errors.New("plain"), ""},
	}
	for _, tc := range cases {
		got := errorHint(tc.err)
		if (tc.want == "" && got != "") || !strings.Contains(got, tc.want) {
			t.Errorf("errorHint(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
