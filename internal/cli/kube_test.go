package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func kubeFixtureConfig(t *testing.T, server string, ca ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfg := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"lab": {Server: server}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"lab": {Token: "kube-fixture"}}, Contexts: map[string]*clientcmdapi.Context{"lab": {Cluster: "lab", AuthInfo: "lab", Namespace: "clients"}}, CurrentContext: "lab"}
	if len(ca) > 0 {
		cfg.Clusters["lab"].CertificateAuthorityData = ca[0]
	}
	if err := clientcmd.WriteToFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKubeObjectsSourceAndContinuationBounds(t *testing.T) {
	var calls atomic.Int32
	var wrongSource atomic.Bool
	var looping atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer kube-fixture" || !strings.HasPrefix(r.URL.Path, "/apis/kudurru.io/v1alpha1/") {
			wrongSource.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		if looping.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZoneList", "metadata": map[string]any{"continue": "repeated"}, "items": []any{}})
			return
		}
		_, _ = io.WriteString(w, `{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"example","uid":"one","resourceVersion":"1"},"spec":{"zoneName":"example.","type":"primary"}}`)
	}))
	defer server.Close()
	file := kubeFixtureConfig(t, server.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	c, ns, err := NewKubeObjects(KubeOptions{ConfigPath: file, Timeout: time.Second, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if ns != "clients" || calls.Load() != 0 {
		t.Fatal("constructor acquired unrelated cluster authority")
	}
	if _, err := c.GetObject(t.Context(), "zones", "", "example"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || wrongSource.Load() {
		t.Fatal("direct request lost original credentials or issued discovery")
	}
	looping.Store(true)
	if _, err := c.ListObjects(t.Context(), "zones", apiclient.ListOptions{PageSize: 1}); err == nil || !strings.Contains(err.Error(), "continuation") {
		t.Fatal("repeated cursor accepted", err)
	}
	if calls.Load() != 3 {
		t.Fatal("unbounded repeated cursor work")
	}
	if _, err := c.GetObject(t.Context(), "secrets", "clients", "private"); err == nil {
		t.Fatal("private kind exposed")
	}
	if _, err := c.GetObject(t.Context(), "zones", "clients", "example"); err == nil {
		t.Fatal("cluster scope inherited namespace")
	}
}

func TestKubeObjectsOriginalCancellationAndResponseBound(t *testing.T) {
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	file := kubeFixtureConfig(t, server.URL)
	c, _, err := NewKubeObjects(KubeOptions{ConfigPath: file, Timeout: time.Second, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.GetObject(ctx, "zones", "", "example"); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled direct read lost original context error")
		}
	case <-time.After(time.Second):
		t.Fatal("original cancellation did not reach source")
	}
	for _, size := range []int{16 << 20, (16 << 20) + 1} {
		reader := &kubeBoundBody{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, size))), remaining: 16 << 20}
		_, err := io.Copy(io.Discard, reader)
		if (err != nil) != (size > 16<<20) {
			t.Fatal("response bound not enforced")
		}
	}
}

func TestKubeObjectsLoadErrorsAndNoAPIFallback(t *testing.T) {
	file := kubeFixtureConfig(t, "https://cluster.invalid")
	for _, opts := range []KubeOptions{
		{ConfigPath: file, Context: "missing", Timeout: time.Second, Getenv: func(string) string { return "" }},
		{ConfigPath: file, Timeout: 0, Getenv: func(string) string { return "" }},
		{ConfigPath: filepath.Join(t.TempDir(), "absent"), Timeout: time.Second, Getenv: func(string) string { return "" }},
		{Timeout: time.Second, Getenv: func(string) string { return "" }},
	} {
		if c, _, err := NewKubeObjects(opts); err == nil {
			_ = c.Close()
			t.Fatal("invalid Kubernetes source accepted")
		}
	}
}
