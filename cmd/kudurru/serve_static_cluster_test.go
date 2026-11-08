package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

type staticHTTPTrace struct {
	t         *testing.T
	transport *http.Transport
}

func (s staticHTTPTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := s.transport.RoundTrip(r)
	if err != nil {
		s.t.Logf("static HTTP %s %s: %v", r.Method, r.URL.Path, err)
	}
	return response, err
}

func staticMountedRequest(t *testing.T, client *http.Client, base, method, path, csrf string, body any, want int, out any) {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-Kudurru-CSRF", csrf)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("static %s %s transport failed: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal("static response read failed", err)
	}
	if response.StatusCode != want {
		var refusal struct {
			Error api.ErrorBody `json:"error"`
		}
		_ = json.Unmarshal(data, &refusal)
		t.Fatalf("static %s %s status=%d want=%d error=%s message=%s", method, path, response.StatusCode, want, refusal.Error.Key, refusal.Error.Message)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		t.Fatal("static response decode failed")
	}
}

func TestStaticClusterRefusesEnrolledDatabase(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Cluster = config.ClusterConfig{Role: "static", Peers: []config.StaticPeer{{Name: cfg.Node.Name, Address: "127.0.0.1:8082"}}}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Node.DataDir, sqlite.FileName)
	opts := sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: cfg.Node.Name}
	st, err := sqlite.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Enroll(t.Context(), replication.Term{ClusterID: "enrolled", Epoch: 1, PrimaryID: "primary", PromotionID: "initial"}); err != nil {
		t.Fatal(err)
	}
	before := st.Authority()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	if opened, _, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler)); err == nil {
		_ = opened.Close()
		t.Fatal("static host routing replaced enrolled authority")
	}
	st, err = sqlite.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if !reflect.DeepEqual(before, st.Authority()) {
		t.Fatal("static refusal changed durable authority")
	}
}

// Two actual serve assemblies have independent SQLite histories and local
// leaders. Only observations poll; every command is dispatched once.
func TestRunServeStaticTwinsReadOnlyPeerLossRestart(t *testing.T) {
	signed := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3})
	t.Setenv(license.EnvKey, signed)
	t.Setenv(license.EnvKeyFile, "")
	body := []byte("blocked.static.example\n")
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer download.Close()
	a, _ := mountedTestConfig(t)
	b, _ := mountedTestConfig(t)
	a.Node.Name, b.Node.Name = "node-a", "node-b"
	a.Listeners.Internal.Address, b.Listeners.Internal.Address = offlineFreeAddress(t, "tcp"), offlineFreeAddress(t, "tcp")
	peers := []config.StaticPeer{{Name: a.Node.Name, Address: a.Listeners.Internal.Address}, {Name: b.Node.Name, Address: b.Listeners.Internal.Address}}
	for _, cfg := range []*config.Config{a, b} {
		cfg.Cluster = config.ClusterConfig{Role: "static", Peers: peers}
		cfg.Blocking.AllowLocalListURLs = true
		// This acceptance has no OIDC provider; omit the unrelated missing-secret
		// provider installed by the general-purpose mount fixture.
		cfg.Auth.IdentityProviders = nil
		// Original source bytes are common to both hosts; independent storage
		// UIDs and serials must never be mistaken for replicated authority.
		source := `[
		 {"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"static"},"spec":{"zoneName":"static.example.","type":"primary","nameservers":["ns.static.example."],"policy":{"notify":"none"}}},
		 {"apiVersion":"kudurru.io/v1alpha1","kind":"DNSRecordSet","metadata":{"name":"seed","namespace":"default"},"spec":{"zoneRef":{"name":"static"},"endpoints":[{"dnsName":"seed.static.example.","recordType":"A","targets":["192.0.2.42"]}]}},
		 {"apiVersion":"kudurru.io/v1alpha1","kind":"BlockList","metadata":{"name":"shared","namespace":"default"},"spec":{"sources":[{"name":"remote","url":"` + download.URL + `","format":"domains"}]}}
		]`
		if err := json.Unmarshal([]byte(source), &cfg.Objects); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	stopA, stopB := startStaticMount(t, a), startStaticMount(t, b)
	defer stopA()
	defer stopB()
	for _, cfg := range []*config.Config{a, b} {
		awaitMountedDNS(t, cfg, "seed.static.example.", "192.0.2.42")
	}
	var clients []*http.Client
	var sessions []api.SessionInfo
	var identities []string
	for _, cfg := range []*config.Config{a, b} {
		awaitMountedDNS(t, cfg, "seed.static.example.", "192.0.2.42")
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		client := &http.Client{Jar: jar, Transport: staticHTTPTrace{t, transport}, Timeout: 5 * time.Second}
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		var session api.SessionInfo
		staticMountedRequest(t, client, base, "POST", "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, 200, &session)
		var info api.ClusterInfo
		staticMountedRequest(t, client, base, "GET", "/cluster", "", nil, 200, &info)
		if !info.ReadOnly || info.Nodes != 2 || info.Leader != cfg.Node.Name || info.Node != cfg.Node.Name || info.ClusterID == "" {
			t.Fatal("static node did not retain local leadership and read-only membership", info)
		}
		identities = append(identities, info.ClusterID)
		nodes := awaitStaticPeerReadiness(t, client, base)
		if len(nodes.Items) != 2 {
			t.Fatal("static peer inventory incomplete")
		}
		for _, node := range nodes.Items {
			if node.Ready == nil || !*node.Ready || node.Error != "" || node.Leader != (node.Name == cfg.Node.Name) {
				t.Fatal("authenticated static peer status or local leader missing", node)
			}
		}
		for _, cmd := range []struct {
			method, path string
			body         any
		}{
			{"PATCH", "/settings", map[string]any{"blocking": map[string]any{"enabled": false}}},
			{"PUT", "/objects/dnszones/remote", map[string]any{}},
			{"POST", "/iam/rolebindings", map[string]any{}},
			{"POST", "/iam/users", map[string]any{}},
			{"POST", "/auth/password", api.PasswordChangeRequest{CurrentPassword: "mounted-test-password-1", NewPassword: "must-not-apply-password-2"}},
		} {
			var refusal struct {
				Error api.ErrorBody `json:"error"`
			}
			staticMountedRequest(t, client, base, cmd.method, cmd.path, session.CSRF, cmd.body, 503, &refusal)
			if refusal.Error.Key != api.KeyReadOnly {
				t.Fatal("static API did not refuse the actual command", cmd.path, refusal.Error.Key)
			}
		}
		clients, sessions = append(clients, client), append(sessions, session)
	}
	if identities[0] == identities[1] {
		t.Fatal("static routing fabricated shared store authority")
	}

	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32), NodeName: a.Node.Name})
	if err != nil {
		t.Fatal(err)
	}
	peerClient := internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: time.Second})
	digest := sha256.Sum256(body)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader, _, err := peerClient.OpenList(ctx, b.Listeners.Internal.Address, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal("actual peer list body unavailable", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, body) {
		t.Fatal("authenticated digest body differs", readErr, closeErr)
	}

	baseA := "http://" + a.Listeners.API.Address + api.BasePath
	var toggled api.BlockingState
	staticMountedRequest(t, clients[0], baseA, "POST", "/dns/blocking", sessions[0].CSRF, map[string]any{"blocking": false, "timer": 60}, 200, &toggled)
	if toggled.Scope != "node" || toggled.Blocking {
		t.Fatal("static toggle did not identify local scope")
	}
	var other api.BlockingState
	staticMountedRequest(t, clients[1], "http://"+b.Listeners.API.Address+api.BasePath, "GET", "/dns/blocking", "", nil, 200, &other)
	if other.Scope != "node" || !other.Blocking {
		t.Fatal("static toggle affected another node")
	}

	stopB()
	awaitMountedDNS(t, a, "seed.static.example.", "192.0.2.42")
	var absent api.List[api.NodeInfo]
	staticMountedRequest(t, clients[0], baseA, "GET", "/nodes", "", nil, 200, &absent)
	if len(absent.Items) != 2 || absent.Items[1].Name != b.Node.Name || absent.Items[1].Ready != nil || absent.Items[1].Error == "" {
		t.Fatal("stopped peer disappeared or claimed readiness")
	}
	stopRestart := startStaticMount(t, b)
	defer stopRestart()
	awaitMountedDNS(t, b, "seed.static.example.", "192.0.2.42")
	rejoined := awaitStaticPeerReadiness(t, clients[0], baseA)
	if len(rejoined.Items) != 2 || rejoined.Items[1].Ready == nil || !*rejoined.Items[1].Ready || rejoined.Items[1].Error != "" {
		t.Fatal("restarted static peer did not return")
	}
	var restarted api.ClusterInfo
	staticMountedRequest(t, clients[1], "http://"+b.Listeners.API.Address+api.BasePath, "GET", "/cluster", "", nil, 200, &restarted)
	if restarted.ClusterID != identities[1] || restarted.Leader != b.Node.Name || !restarted.ReadOnly {
		t.Fatal("restart changed static store identity or role")
	}
}

func startStaticMount(t *testing.T, cfg *config.Config) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() { defer close(done); runErr = runServe(ctx, cfg, "") }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
				if runErr != nil {
					t.Error("static server stopped", cfg.Node.Name, runErr)
				}
			case <-time.After(15 * time.Second):
				t.Error("static server did not join", cfg.Node.Name)
			}
		})
	}
	t.Cleanup(stop)
	probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
	defer probeCancel()
	client := &http.Client{Timeout: time.Second}
	var last string
	for {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+cfg.Listeners.Ops.Address+"/healthz/ready", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err == nil {
			data, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
			_ = response.Body.Close()
			last = string(data)
			if response.StatusCode == http.StatusOK {
				return stop
			}
		}
		select {
		case <-done:
			t.Fatal("static server exited before readiness", cfg.Node.Name, runErr)
		case <-probeCtx.Done():
			t.Fatal("static server never ready", cfg.Node.Name, last)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Complete-publication readiness is an observation, not a command receipt.
// Status writes and login enrollment can briefly retire a previously ready cut.
func awaitStaticPeerReadiness(t *testing.T, client *http.Client, base string) api.List[api.NodeInfo] {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/nodes", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		var nodes api.List[api.NodeInfo]
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && json.Unmarshal(data, &nodes) == nil && len(nodes.Items) == 2 {
				ready := true
				for _, n := range nodes.Items {
					ready = ready && n.Ready != nil && *n.Ready && n.Error == ""
				}
				if ready {
					return nodes
				}
			}
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("peer observation refused", response.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("static peers never simultaneously ready", nodes)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
