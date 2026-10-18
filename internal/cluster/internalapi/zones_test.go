package internalapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientZones(t *testing.T) {
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	var serve atomic.Bool
	serve.Store(true)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+creds.Token() {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if !serve.Load() || r.URL.Path != PathZones {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"node":"node-a","zones":[{"name":"example.com.","serial":2026101101,"type":"primary"}]}`))
	}))
	srv.TLS = creds.ServerTLS()
	srv.StartTLS()
	defer srv.Close()
	c := NewClient(creds, ClientOptions{Timeout: 5 * time.Second})
	addr := srv.Listener.Addr().String()
	zl, err := c.Zones(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if zl.Node != "node-a" || len(zl.Zones) != 1 || zl.Zones[0] != (Zone{Name: "example.com.", Serial: 2026101101, Type: "primary"}) {
		t.Errorf("got %+v", zl)
	}
	serve.Store(false)
	if _, err := c.Zones(context.Background(), addr); !errors.Is(err, ErrZonesUnsupported) {
		t.Errorf("404: %v, want ErrZonesUnsupported", err)
	}
}
