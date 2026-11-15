package auditkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestSourceAuthenticatedBoundedOriginalTrust(t *testing.T) {
	ring := newTestRing(t)
	raw, _ := ring.EncodePrivate()
	var mode atomic.Int32
	var calls atomic.Int32
	var cluster atomic.Value
	cluster.Store("")
	var mu sync.Mutex
	revision := "1"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/namespaces/default/secrets/trust" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-only" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusNotFound)
			return
		case 2:
			w.Header().Set("Location", "/redirect")
			w.WriteHeader(http.StatusFound)
			return
		case 3:
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxSecretResponse+1))
			return
		case 4:
			_, _ = w.Write([]byte(`{"metadata":`))
			return
		case 5:
			<-r.Context().Done()
			return
		case 6:
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		metadata := map[string]string{"uid": "uid-original", "resourceVersion": revision, "name": "trust", "namespace": "default"}
		data := raw
		switch mode.Load() {
		case 7:
			metadata["namespace"] = "other"
		case 8:
			metadata["uid"] = ""
		case 9:
			metadata["name"] = "other"
		case 10:
			data = []byte(`{"version":9}`)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": metadata, "data": map[string][]byte{"ring.json": data}})
	}))
	defer api.Close()
	source, err := NewSource(&rest.Config{Host: api.URL, BearerToken: "fixture-only"}, "default", "trust", func() string { return cluster.Load().(string) })
	if err != nil || calls.Load() != 0 {
		t.Fatal("constructor contacted API")
	}
	if _, _, err := source.Load(t.Context()); !errors.Is(err, ErrUnavailable) || calls.Load() != 0 {
		t.Fatal("unbound source performed network work")
	}
	cluster.Store("cluster-original")
	loaded, id, err := source.Load(t.Context())
	if err != nil || loaded.Count() != 1 || id == "" {
		t.Fatal("authenticated capture failed")
	}
	if source.Current(t.Context(), id) != nil {
		t.Fatal("unchanged cut refused")
	}
	mu.Lock()
	revision = "2"
	mu.Unlock()
	if !errors.Is(source.Current(t.Context(), id), ErrUnavailable) {
		t.Fatal("superseded trust repaired")
	}
	for _, m := range []int32{1, 2, 3, 4, 6, 7, 8, 9, 10} {
		mode.Store(m)
		before := calls.Load()
		_, _, err := source.Load(t.Context())
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "fixture-only") {
			t.Fatalf("mode %d did not fail safely", m)
		}
		if m == 2 && calls.Load() != before+1 {
			t.Fatal("followed redirect")
		}
	}
	mode.Store(5)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	_, _, err = source.Load(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("did not honor caller deadline")
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, _, err = source.Load(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("did not honor cancellation")
	}
	mode.Store(0)
	if _, _, err := source.Load(t.Context()); err != nil {
		t.Fatal("could not recover after unavailable startup")
	}
	wrong, _ := NewSource(&rest.Config{Host: api.URL, BearerToken: "wrong-fixture"}, "default", "trust", func() string { return "cluster-original" })
	if _, _, err := wrong.Load(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("wrong auth accepted")
	}
	cluster.Store("cluster-replacement")
	if _, _, err := source.Load(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("rebound source to replacement cluster")
	}
	cluster.Store("cluster-original")
	if _, _, err := source.Load(t.Context()); err != nil {
		t.Fatal("original recovery refused")
	}
	mu.Lock()
	other, _ := New("other-cluster", "default", nil, nil, time.Now())
	raw, _ = other.EncodePrivate()
	mu.Unlock()
	if _, _, err := source.Load(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("accepted ring from wrong cluster")
	}
}

func TestSourceTransportTimeoutAndUnavailable(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	source, err := NewSource(&rest.Config{Host: api.URL}, "default", "trust", func() string { return "original" })
	if err != nil {
		api.Close()
		t.Fatal("source constructor")
	}
	short, shortErr := NewSource(&rest.Config{Host: api.URL, Timeout: 20 * time.Millisecond}, "default", "trust", func() string { return "original" })
	if shortErr != nil || short.client.Timeout != 20*time.Millisecond {
		api.Close()
		t.Fatal("weakened original transport deadline")
	}
	if source.client.Timeout != 3*time.Second {
		api.Close()
		t.Fatal("lost production transport bound")
	}
	// Use the actual configured bound, rather than a caller deadline masking it.
	start := time.Now()
	_, _, err = source.Load(t.Context())
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 5*time.Second {
		api.Close()
		t.Fatal("transport timeout unbounded")
	}
	api.Close()
	if _, _, err := source.Load(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unreachable authority accepted")
	}
}
