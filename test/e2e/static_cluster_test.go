package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

type staticProcessClient struct {
	client *http.Client
	base   string
	csrf   string
}

// Every command is one original attempt, with no body replay or retry. Read
// readiness observation below may poll; it never resubmits a refused mutation.
func (c *staticProcessClient) request(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.GetBody = nil
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		r.Header.Set("X-Kudurru-CSRF", c.csrf)
	}
	response, err := c.client.Do(r)
	if err != nil {
		t.Fatalf("static process %s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(response.Body, 128<<10))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		var problem struct {
			Error api.ErrorBody `json:"error"`
		}
		_ = json.Unmarshal(data, &problem)
		t.Fatalf("static process %s %s: status %d want %d, error %s", method, path, response.StatusCode, want, problem.Error.Key)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
}

func staticProcessPeers(t *testing.T, c *staticProcessClient) []api.NodeInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var last []api.NodeInfo
	for {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/nodes", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := c.client.Do(r)
		if err == nil {
			var nodes api.List[api.NodeInfo]
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&nodes)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && decodeErr == nil {
				last = nodes.Items
				ready := len(last) == 2
				for _, node := range last {
					ready = ready && node.Ready != nil && *node.Ready && node.Error == ""
				}
				if ready {
					return last
				}
			} else if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("static process peer observation refused", response.StatusCode, decodeErr)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("static process peers not ready", last)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func TestStaticClusterProcessesReadOnlyPeerLossRestart(t *testing.T) {
	kudurrutest.RequireE2E(t)
	bin, env := licensedNodeFixture(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	env = append(env, "KUDURRU_ENCRYPTION_KEY="+hex.EncodeToString(key))
	body := []byte("blocked.static-process.example\n")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer source.Close()
	objects := []any{
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "static"}, "spec": map[string]any{"zoneName": "static-process.example.", "type": "primary", "nameservers": []string{"ns.static-process.example."}, "policy": map[string]any{"notify": "none"}}},
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": map[string]any{"name": "seed", "namespace": "default"}, "spec": map[string]any{"zoneRef": map[string]any{"name": "static"}, "endpoints": []any{map[string]any{"dnsName": "seed.static-process.example.", "recordType": "A", "targets": []string{"192.0.2.42"}}}}},
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList", "metadata": map[string]any{"name": "shared", "namespace": "default"}, "spec": map[string]any{"sources": []any{map[string]any{"name": "remote", "url": source.URL, "format": "domains"}}}},
	}
	dir := t.TempDir()
	a, acfg := kudurrutest.NewNodeSpec(t, dir, "static-a")
	b, bcfg := kudurrutest.NewNodeSpec(t, dir, "static-b")
	peers := []any{map[string]any{"name": a.Name, "address": a.Ports.Internal}, map[string]any{"name": b.Name, "address": b.Ports.Internal}}
	specs := []kudurrutest.NodeSpec{a, b}
	for i, cfg := range []map[string]any{acfg, bcfg} {
		cfg["cluster"] = map[string]any{"role": "static", "peers": peers}
		cfg["objects"] = objects
		cfg["blocking"] = map[string]any{"allowLocalListURLs": true}
		cfg["querylog"] = map[string]any{"enabled": false}
		cfg["log"] = map[string]any{"level": "error", "format": "text"}
		cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
		kudurrutest.WriteConfig(t, specs[i].Config, cfg)
	}
	nodes := []*kudurrutest.Node{kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env}), kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})}
	checkDNS := func(node *kudurrutest.Node) {
		t.Helper()
		for _, tcp := range []bool{false, true} {
			r, err := exchange(node.Ports.DNS, "seed.static-process.example.", dns.TypeA, tcp)
			if err != nil || r.Rcode != dns.RcodeSuccess || firstA(r) != "192.0.2.42" {
				t.Fatal("static process DNS lost admitted local data", node.Name, tcp, err)
			}
		}
	}
	var clients []*staticProcessClient
	var identities []string
	for _, node := range nodes {
		checkDNS(node)
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		client := &staticProcessClient{client: &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}, base: "http://" + node.Ports.API + api.BasePath}
		var session api.SessionInfo
		client.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "audit-e2e-password-1"}, http.StatusOK, &session) // #nosec G101 -- disposable loopback test credential created by this fixture
		client.csrf = session.CSRF
		inventory := staticProcessPeers(t, client)
		for _, peer := range inventory {
			if peer.Leader != (peer.Name == node.Name) {
				t.Fatal("static process did not retain local leadership")
			}
		}
		var cluster api.ClusterInfo
		client.request(t, http.MethodGet, "/cluster", nil, http.StatusOK, &cluster)
		if !cluster.ReadOnly || cluster.Leader != node.Name || cluster.Node != node.Name || cluster.Nodes != 2 || cluster.ClusterID == "" {
			t.Fatal("static process lost configured membership", cluster)
		}
		identities = append(identities, cluster.ClusterID)
		for _, path := range []string{"/settings", "/objects/dnszones/static"} {
			method := http.MethodPut
			if path == "/settings" {
				method = http.MethodPatch
			}
			var problem struct {
				Error api.ErrorBody `json:"error"`
			}
			client.request(t, method, path, map[string]any{}, http.StatusServiceUnavailable, &problem)
			if problem.Error.Key != api.KeyReadOnly {
				t.Fatal("static process did not apply the read-only guard", path, problem.Error.Key)
			}
		}
		clients = append(clients, client)
	}
	if identities[0] == identities[1] {
		t.Fatal("static processes fabricated a shared store identity")
	}
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: key, NodeName: a.Name})
	if err != nil {
		t.Fatal(err)
	}
	peerClient := internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 5 * time.Second})
	digest := sha256.Sum256(body)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader, _, err := peerClient.OpenList(ctx, b.Ports.Internal, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, body) {
		t.Fatal("static peer digest body differs", readErr, closeErr)
	}
	var toggled api.BlockingState
	clients[0].request(t, http.MethodPost, "/dns/blocking", map[string]any{"blocking": false, "timer": 60}, http.StatusOK, &toggled)
	if toggled.Scope != "node" || toggled.Blocking {
		t.Fatal("static process toggle lost local scope")
	}
	var other api.BlockingState
	clients[1].request(t, http.MethodGet, "/dns/blocking", nil, http.StatusOK, &other)
	if other.Scope != "node" || !other.Blocking {
		t.Fatal("static process toggle affected another process")
	}
	nodes[0].Stop(15 * time.Second)
	if code, err := nodes[0].Wait(time.Second); err != nil || code != 0 {
		t.Fatal("static process did not exit cleanly", code, err)
	}
	checkDNS(nodes[1])
	var absent api.List[api.NodeInfo]
	clients[1].request(t, http.MethodGet, "/nodes", nil, http.StatusOK, &absent)
	if len(absent.Items) != 2 || absent.Items[0].Name != a.Name || absent.Items[0].Ready != nil || absent.Items[0].Error == "" {
		t.Fatal("stopped static process advertised readiness")
	}
	restarted := kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	checkDNS(restarted)
	_ = staticProcessPeers(t, clients[1])
	var restored api.ClusterInfo
	clients[0].request(t, http.MethodGet, "/cluster", nil, http.StatusOK, &restored)
	if restored.ClusterID != identities[0] || restored.Leader != a.Name || !restored.ReadOnly {
		t.Fatal("static process restart changed authority", restored)
	}
}
