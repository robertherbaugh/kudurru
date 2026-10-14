package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func TestClientsScopeRouting(t *testing.T) {
	f := newClusterLogFixture(t)
	now := time.Now().Add(-time.Minute)
	// Include a blocked-only local client and a mixed remote client so
	// both rankings must be routed and merged, with shared totals counted once.
	local := []*querylog.Event{
		qev(4, now, "192.0.2.2", "ads.test", pipeline.StatusBlockedList, "ads"),
		qev(5, now.Add(time.Second), "192.0.2.2", "ads.test", pipeline.StatusBlockedList, "ads"),
	}
	remote := []*querylog.Event{
		qev(4, now, "192.0.2.9", "remote.test", pipeline.StatusForwarded, ""),
		qev(5, now.Add(time.Second), "192.0.2.9", "ads.test", pipeline.StatusBlockedList, "ads"),
	}
	for _, ev := range remote {
		ev.Node = "node-b"
	}
	if err := f.a.Write(local); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Write(remote); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, scope string
		nodes              []string
		counts             map[string][2]int64
	}{
		{"legacy local", "", "node", []string{"node-a"}, map[string][2]int64{"10.0.0.1": {3, 1}, "192.0.2.2": {2, 2}}},
		{"explicit local", "scope=node&node=node-a", "node", []string{"node-a"}, map[string][2]int64{"10.0.0.1": {3, 1}, "192.0.2.2": {2, 2}}},
		{"peer", "scope=node&node=node-b", "node", []string{"node-b"}, map[string][2]int64{"10.0.0.1": {3, 1}, "192.0.2.9": {2, 1}}},
		{"cluster", "scope=cluster", "cluster", []string{"node-a", "node-b"}, map[string][2]int64{"10.0.0.1": {6, 2}, "192.0.2.2": {2, 2}, "192.0.2.9": {2, 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.calls.Load()
			var body ClientsBody
			f.h.do("GET", "/api/v1/clients?window=1h&"+tc.query, "viewer", nil).want(200).json(&body)
			counts := map[string][2]int64{}
			for _, item := range body.Items {
				counts[item.Key] = [2]int64{item.Count, item.Blocked}
				if item.LastSeen == nil {
					t.Fatal("last_seen missing")
				}
			}
			var nodes []string
			for _, node := range body.Nodes {
				nodes = append(nodes, node.Name)
				if !node.OK {
					t.Fatalf("unexpected failed node %+v", node)
				}
			}
			if !reflect.DeepEqual(counts, tc.counts) || !reflect.DeepEqual(nodes, tc.nodes) || body.Scope.Scope != tc.scope || body.Partial || body.Continue != "" {
				t.Fatalf("inventory %+v; want counts %v, scope %s, nodes %v", body, tc.counts, tc.scope, tc.nodes)
			}
			if tc.nodes[0] == "node-a" && len(tc.nodes) == 1 && f.calls.Load() != before {
				t.Fatal("local inventory read the peer")
			}
		})
	}
	for path, status := range map[string]int{
		"scope=node&node=unknown":   404,
		"scope=invalid":             400,
		"scope=cluster&node=node-b": 400,
	} {
		f.h.do("GET", "/api/v1/clients?"+path, "viewer", nil).want(status)
	}
}

func TestClientsPartialScope(t *testing.T) {
	f := newClusterLogFixture(t)
	peers, err := f.h.srv.opts.Membership.Peers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.h.srv.opts.Membership = staticMembership{peers: append(peers, cluster.Peer{Name: "offline"})}
	var body ClientsBody
	f.h.do("GET", "/api/v1/clients?scope=cluster", "viewer", nil).want(200).json(&body)
	if !body.Partial || len(body.Nodes) != 3 || body.Nodes[2].Name != "offline" || body.Nodes[2].OK || body.Nodes[2].Error == "" || len(body.Items) != 1 || body.Items[0].Count != 6 || body.Items[0].Blocked != 2 {
		t.Fatalf("partial inventory %+v", body)
	}
	f.h.do("GET", "/api/v1/clients?scope=node&node=offline", "viewer", nil).want(503)
}

type inventoryTransport func(*http.Request) (*http.Response, error)

func (f inventoryTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestClientsPartialRankingScope(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "permitted ranking fails", true: "blocked ranking fails"}[blocked], func(t *testing.T) {
			f := newClusterLogFixture(t)
			creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "node-b"})
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{TLSClientConfig: creds.ClientTLS()}
			t.Cleanup(transport.CloseIdleConnections)
			f.h.srv.opts.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{Transport: inventoryTransport(func(req *http.Request) (*http.Response, error) {
				var q internalapi.QuerylogRequest
				if err := json.Unmarshal([]byte(req.URL.Query().Get("q")), &q); err != nil {
					return nil, err
				}
				if q.Kind == "clients" && q.Blocked == blocked {
					return nil, errors.New("ranking unavailable")
				}
				return transport.RoundTrip(req)
			})})
			var body ClientsBody
			f.h.do("GET", "/api/v1/clients?scope=cluster", "viewer", nil).want(200).json(&body)
			if !body.Partial || len(body.Nodes) != 2 || body.Nodes[1].Name != "node-b" || body.Nodes[1].OK || body.Nodes[1].Error == "" || len(body.Items) != 1 || body.Items[0].Count != 6 || body.Items[0].Blocked != 2 {
				t.Fatalf("lost a failure from one ranking: %+v", body)
			}
		})
	}
}

func TestClientsCentralRouting(t *testing.T) {
	// The existing central-routing fixture uses a second reader in place
	// of PostgreSQL. The local reader is empty, as on a central-only deployment.
	f := newClusterLogFixture(t)
	empty, _ := openNodeLog(t, "node-a", nil)
	f.h.srv.opts.Querylog, f.h.srv.opts.CentralQuerylog = empty.Reader(), f.b.Reader()
	before := f.calls.Load()
	var body ClientsBody
	f.h.do("GET", "/api/v1/clients?scope=cluster", "viewer", nil).want(200).json(&body)
	if len(body.Items) != 1 || body.Items[0].Count != 3 || body.Items[0].Blocked != 1 || f.calls.Load() != before {
		t.Fatalf("central inventory %+v; peer calls %d", body, f.calls.Load()-before)
	}
	for _, scope := range []string{"", "scope=node&node=node-a"} {
		f.h.do("GET", "/api/v1/clients?"+scope, "viewer", nil).want(200).json(&body)
		if len(body.Items) != 0 || body.Scope.Scope != "node" || body.Nodes[0].Name != "node-a" {
			t.Fatalf("local inventory %+v", body)
		}
	}
	// Central reads also work without any local query log.
	f.h.srv.opts.Querylog = nil
	f.h.do("GET", "/api/v1/clients?scope=cluster", "viewer", nil).want(200).json(&body)
	if len(body.Items) != 1 {
		t.Fatalf("central-only inventory %+v", body)
	}
	f.h.do("GET", "/api/v1/clients", "viewer", nil).want(503)
}
