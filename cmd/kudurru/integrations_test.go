package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationAdminCLIActualRouter(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	server := httptest.NewTLSServer(integrationAdminRouter(t, f))
	defer server.Close()
	run := func(args ...string) (string, error) {
		o := newCLIOpts()
		o.getenv = func(string) string { return "" }
		o.httpClient = server.Client()
		root := newRootWith(o)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--cli-config", filepath.Join(t.TempDir(), "contexts.yaml"), "--server", server.URL, "--token", f.token, "-o", "json", "integrations"}, args...))
		err := root.ExecuteContext(t.Context())
		return out.String(), err
	}
	for _, args := range [][]string{{"list"}, {"get", "manual"}} {
		out, err := run(args...)
		if err != nil || !strings.Contains(out, string(f.d.UID)) || strings.Contains(out, f.path) {
			t.Fatal("CLI lost original review or disclosed private source", err != nil)
		}
	}
	before := f.calls.Load()
	if _, err := run("check", "manual"); err == nil || f.calls.Load() != before {
		t.Fatal("missing review was fetched or executed")
	}
	out, err := run("check", "manual", "--uid", string(f.d.UID), "--revision", f.d.ResourceVersion)
	if err != nil || !strings.Contains(out, `"completed": true`) || f.calls.Load() != before+1 {
		t.Fatal("CLI real checked request failed", err != nil)
	}
}

func TestIntegrationCLIRefusesKubernetesTransport(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	o := newCLIOpts()
	o.getenv = func(string) string { return "" }
	cmd := newRootWith(o)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--transport", "kubernetes", "--server", server.URL, "integrations", "list"})
	if err := cmd.ExecuteContext(t.Context()); err == nil || calls != 0 {
		t.Fatal("integration CLI bypassed its API transport")
	}
}
