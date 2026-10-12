package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func TestNodesPeerStatus(t *testing.T) {
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	ready, unready := true, false
	for _, tc := range []struct {
		name   string
		status internalapi.Status
	}{
		{name: "ready", status: internalapi.Status{Ready: &ready, Version: "peer-version", State: "live"}},
		{name: "unready snapshot", status: internalapi.Status{Ready: &unready, Unready: []string{"zonecache: unsynced"}, Version: "peer-version", State: "fromSnapshot", Snapshot: "3m0s"}},
		{name: "older peer", status: internalapi.Status{Version: "old-version", State: "live"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			status.Node = "node-b"
			peer, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, Status: func(context.Context) internalapi.Status { return status }})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewUnstartedServer(peer.Handler())
			ts.TLS = creds.ServerTLS()
			ts.StartTLS()
			t.Cleanup(ts.Close)
			failed := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
			failed.TLS = creds.ServerTLS()
			failed.StartTLS()
			t.Cleanup(failed.Close)
			h := newHarness(t, func(o *Options) {
				o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
				o.Membership = staticMembership{peers: []cluster.Peer{
					{Name: "node-b", Address: strings.TrimPrefix(ts.URL, "https://")},
					{Name: "node-c", Address: strings.TrimPrefix(failed.URL, "https://")},
					{Name: "node-a", Self: true},
				}}
			})
			var nodes struct {
				Items []NodeInfo `json:"items"`
			}
			h.do(http.MethodGet, "/api/v1/nodes", "admin", nil).want(http.StatusOK).json(&nodes)
			if len(nodes.Items) != 3 {
				t.Fatalf("nodes = %+v", nodes.Items)
			}
			self, got, failure := nodes.Items[0], nodes.Items[1], nodes.Items[2]
			if self.Name != "node-a" || !self.Self || self.Ready == nil || self.Version != "v0.1.0-test" {
				t.Fatalf("self = %+v", self)
			}
			if got.Name != "node-b" || got.Self || !got.Leader || !reflect.DeepEqual(got.Ready, status.Ready) || !reflect.DeepEqual(got.Unready, status.Unready) || got.Version != status.Version || got.State != status.State || got.Snapshot != status.Snapshot || got.Error != "" {
				t.Fatalf("peer = %+v; status = %+v", got, status)
			}
			if failure.Name != "node-c" || failure.Ready != nil || !strings.Contains(failure.Error, "503") {
				t.Fatalf("failed peer must remain listed with unknown readiness: %+v", failure)
			}
			var info ClusterInfo
			h.do(http.MethodGet, "/api/v1/cluster", "admin", nil).want(http.StatusOK).json(&info)
			if info.Nodes != 3 || info.Leader != "node-b" {
				t.Fatalf("cluster = %+v", info)
			}
		})
	}
}

func TestNodesPeerStatusTimeout(t *testing.T) {
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	peer.TLS = creds.ServerTLS()
	peer.StartTLS()
	t.Cleanup(peer.Close)
	h := newHarness(t, func(o *Options) {
		o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
		o.Membership = staticMembership{peers: []cluster.Peer{
			{Name: "node-a", Self: true},
			{Name: "node-b", Address: strings.TrimPrefix(peer.URL, "https://")},
			{Name: "node-c", Address: strings.TrimPrefix(peer.URL, "https://")},
		}}
	})
	start := time.Now()
	var nodes struct {
		Items []NodeInfo `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/nodes", "admin", nil).want(http.StatusOK).json(&nodes)
	if elapsed := time.Since(start); elapsed > 3500*time.Millisecond {
		t.Fatalf("peer requests should time out concurrently: %v", elapsed)
	}
	if len(nodes.Items) != 3 {
		t.Fatalf("nodes = %+v", nodes.Items)
	}
	for _, n := range nodes.Items[1:] {
		if n.Ready != nil || !strings.Contains(n.Error, "context deadline exceeded") {
			t.Fatalf("timed-out peer = %+v", n)
		}
	}
}
