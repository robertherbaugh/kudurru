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

// The read-back probe is a read: it needs no --allow-mutations (D380).
func TestIntegrationConformanceCLIReadBackFlag(t *testing.T) {
	var posts atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/external.dns.webhook+json;version=1")
		switch {
		case r.URL.Path == "/":
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/records" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `[{"dnsName":"a.example.test","targets":["192.0.2.1"],"recordType":"A"}]`)
		default:
			if r.URL.Path == "/records" {
				posts.Add(1)
			}
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer s.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	expect := filepath.Join(dir, "expect.json")
	if err := os.WriteFile(expect, []byte(`{"contain":[{"dnsName":"a.example.test","targets":["192.0.2.1"],"recordType":"A"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := newCLIOpts()
	o.getenv = func(string) string { return "" }
	root := newRootWith(o)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--cli-config", filepath.Join(dir, "missing"), "integrations", "webhook", "conformance", s.URL, "--ca-file", ca, "--expect-endpoints-file", expect})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(out.String(), `{"name":"records_contain","status":"passed"}`) || posts.Load() != 0 {
		t.Fatal("read-back flag", out.String())
	}
}
