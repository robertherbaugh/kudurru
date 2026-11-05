package main

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIntegrationConformanceCLIExplicitTarget(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("ambient API credential forwarded")
		}
		w.Header().Set("Content-Type", "application/external.dns.webhook+json;version=1")
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, `{}`)
		} else {
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer s.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	o := newCLIOpts()
	o.getenv = func(string) string { return "" }
	root := newRootWith(o)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--cli-config", filepath.Join(t.TempDir(), "missing"), "--server", "https://unrelated.invalid", "--token", "private-api-test-token", "integrations", "webhook", "conformance", s.URL, "--ca-file", ca})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !strings.Contains(out.String(), `"complete":false`) || strings.Contains(out.String(), "private-api-test-token") {
		t.Fatal("standalone CLI contract failed")
	}
}

func TestIntegrationConformanceCLIRequiresExplicitMutation(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer s.Close()
	file := filepath.Join(t.TempDir(), "changes.json")
	if err := os.WriteFile(file, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := newCLIOpts()
	o.getenv = func(string) string { return "" }
	root := newRootWith(o)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"integrations", "webhook", "conformance", s.URL, "--changes-file", file})
	if err := root.ExecuteContext(t.Context()); err == nil || calls.Load() != 0 {
		t.Fatal("unapproved mutation sent")
	}
}
