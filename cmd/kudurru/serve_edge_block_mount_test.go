package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// edgeBlockRoot is a mounted SQLite runServe with an admin session and an
// internal client holding the cluster credentials an edge would hold.
type edgeBlockRoot struct {
	cfg      *config.Config
	client   *http.Client
	base     string
	csrf     string
	internal *internalapi.Client
}

func startEdgeBlockRoot(t *testing.T, mutate func(*config.Config)) *edgeBlockRoot {
	t.Helper()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	if mutate != nil {
		mutate(cfg)
	}
	licenseKey := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureEdge, license.FeatureAudit}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, licenseKey) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("root shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("root shutdown did not join")
		}
	})
	edgeBlockWait(t, "root ready", func() bool { return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK })
	jar, _ := cookiejar.New(nil)
	r := &edgeBlockRoot{cfg: cfg, client: &http.Client{Jar: jar, Timeout: 10 * time.Second}, base: "http://" + cfg.Listeners.API.Address + api.BasePath}
	var login api.SessionInfo
	controlMountedRequest(t, r.client, r.base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	r.csrf = login.CSRF
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32), NodeName: "edge-block-edge"})
	if err != nil {
		t.Fatal(err)
	}
	r.internal = internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 3 * time.Second})
	return r
}

func edgeBlockWait(t *testing.T, label string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("timed out: " + label)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// command sends one original command after the publication has been quiet
// for half a second: an ordinary background publication can supersede a
// request's original cut, which production correctly refuses. The command
// is never replayed.
func (r *edgeBlockRoot) command(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	publication := func() (uint64, bool) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		z, err := r.internal.Zones(ctx, r.cfg.Listeners.Internal.Address)
		if err != nil || z.Resolver == nil {
			return 0, false
		}
		return z.Resolver.Publication, true
	}
	var before uint64
	var since time.Time
	edgeBlockWait(t, "quiet publication", func() bool {
		p, ok := publication()
		if !ok {
			since = time.Time{}
			return false
		}
		if p != before || since.IsZero() {
			before, since = p, time.Now()
			return false
		}
		return time.Since(since) >= 500*time.Millisecond
	})
	controlMountedRequest(t, r.client, r.base, method, path, r.csrf, body, want, out)
}

// served reads the internal route once. A publication that supersedes the
// handler's lease answers 503 (the edge keeps its last set and polls again),
// so callers poll; an empty Owner marks such a refused read.
func (r *edgeBlockRoot) served(t *testing.T) internalapi.EdgeBlockList {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	l, err := r.internal.EdgeBlocks(ctx, r.cfg.Listeners.Internal.Address)
	if err != nil {
		return internalapi.EdgeBlockList{}
	}
	return l
}

// TestRunServeEdgeBlocksSQLite: the mounted SQLite root stores an operator
// edge block through the API with its audit row, serves it to an edge on
// the authenticated internal listener from the next publication, refuses
// out-of-bound and duplicate requests, and serves the revoke.
func TestRunServeEdgeBlocksSQLite(t *testing.T) {
	r := startEdgeBlockRoot(t, nil)
	var initial internalapi.EdgeBlockList
	edgeBlockWait(t, "first complete publication served", func() bool { initial = r.served(t); return initial.Owner != "" })
	if len(initial.Blocks) != 0 || initial.Revision != 0 {
		t.Fatalf("initial served set %+v", initial)
	}
	r.command(t, http.MethodPost, "/edge/blocks", map[string]any{"prefix": "10.0.0.0/8", "seconds": 600}, http.StatusBadRequest, nil)
	var created api.EdgeBlock
	r.command(t, http.MethodPost, "/edge/blocks", map[string]any{"prefix": "192.0.2.77/24", "seconds": 600, "reason": "query flood"}, http.StatusCreated, &created)
	if created.Prefix != "192.0.2.0/24" || created.CreatedBy == "" || created.Reason != "query flood" || created.Until.Sub(created.CreatedAt) != 10*time.Minute {
		t.Fatalf("created %+v", created)
	}
	var owner string
	edgeBlockWait(t, "block served to edges", func() bool {
		l := r.served(t)
		owner = l.Owner
		return owner != "" && l.Revision == 1 && len(l.Blocks) == 1 && l.Blocks[0].ID == created.ID && l.Blocks[0].Until.Equal(created.Until)
	})
	r.command(t, http.MethodPost, "/edge/blocks", map[string]any{"prefix": "192.0.2.1/24", "seconds": 900}, http.StatusConflict, nil)
	var list api.EdgeBlockList
	controlMountedRequest(t, r.client, r.base, http.MethodGet, "/edge/blocks", "", nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID || list.Revision != "1" {
		t.Fatalf("list %+v", list)
	}
	var revoked api.EdgeBlock
	r.command(t, http.MethodDelete, "/edge/blocks/"+created.ID, nil, http.StatusOK, &revoked)
	edgeBlockWait(t, "revoke served to edges", func() bool { l := r.served(t); return l.Owner == owner && l.Revision == 2 && len(l.Blocks) == 0 })
	r.command(t, http.MethodDelete, "/edge/blocks/"+created.ID, nil, http.StatusNotFound, nil)
	var page api.AuditPage
	controlMountedRequest(t, r.client, r.base, http.MethodGet, "/audit?order=desc", "", nil, http.StatusOK, &page)
	seen := map[string]bool{}
	for _, e := range page.Items {
		if (e.Action == "edgeblock.create" || e.Action == "edgeblock.revoke") && e.Resource.Name == created.ID && e.Actor.Name == "admin" {
			seen[e.Action] = true
		}
	}
	if !seen["edgeblock.create"] || !seen["edgeblock.revoke"] {
		t.Fatalf("audit rows missing: %+v", seen)
	}
}

// failingBlocks stands for an edge that cannot reach any backend.
type failingBlocks struct{}

func (failingBlocks) EdgeBlocks(context.Context, string) (internalapi.EdgeBlockList, error) {
	return internalapi.EdgeBlockList{}, errors.New("backend unreachable")
}

// TestRunServeEdgeBlockLiveDnsdist is the D388 acceptance against the pinned
// dnsdist image (KUDURRU_F43_LIVE=1; KUDURRU_F43_RENDERER=lua selects the
// Lua renderer): a block pushed through the mounted API drops that client's
// queries at the real edge and not another client's, a revoke lets the
// client through again, a restarted edge with no reachable backend keeps
// dropping until the expiry, and dnsdist lets the client through after it.
func TestRunServeEdgeBlockLiveDnsdist(t *testing.T) {
	if os.Getenv("KUDURRU_F43_LIVE") != "1" {
		t.Skip("set KUDURRU_F43_LIVE=1 for owned pinned dnsdist acceptance")
	}
	parent := os.Getenv("KUDURRU_F43_WORKDIR")
	if parent == "" {
		t.Fatal("KUDURRU_F43_WORKDIR must name an owned scratch parent")
	}
	dir, err := os.MkdirTemp(parent, "edge-block-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) }) //nolint:gosec // only this fresh owned fixture
	t.Setenv("TMPDIR", dir)
	name := "kudurru-edge-dynblock-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	docker := []string{"--host", "unix:///tmp/kudurru-lab-docker.sock"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", append(docker, "rm", "--force", name)...).Run() //nolint:gosec // fixed daemon, unique owned name
	})
	fixture, _ := json.Marshal(map[string]string{"name": name, "image": "sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e"})
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0o600); err != nil { //nolint:gosec // fresh private fixture directory
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile("../../ee/edge/dnsdist/testdata/console-client.py")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "dnsdist-client")
	if err := os.WriteFile(binary, wrapper, 0o700); err != nil { //nolint:gosec // private executable test wrapper, fixed tracked contents
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		m.RecursionAvailable = true
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.IPv4(192, 0, 2, 80)}}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	t.Cleanup(func() { _ = upstream.Shutdown() })
	secret := bytes.Repeat([]byte{0x39}, 32)
	r := startEdgeBlockRoot(t, func(cfg *config.Config) {
		cfg.DNSSEC.Enabled = false
		cfg.Cache.MaxEntries = 0
		cfg.RateLimit.Enabled = false
		cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(5 * time.Second)}}
		cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
		cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(dir, "proxy-secret")
		if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	var renderer dnsdist.Renderer = dnsdist.YAMLRenderer{}
	if os.Getenv("KUDURRU_F43_RENDERER") == "lua" {
		renderer = dnsdist.LuaRenderer{}
	}
	_, port, _ := net.SplitHostPort(offlineFreeAddress(t, "tcp"))
	listenPort, _ := strconv.Atoi(port)
	edgeAddr := net.JoinHostPort("127.0.0.1", port)
	options := edge.Options{ServerID: "edge-block-live", Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: listenPort}, API: offlineFreeAddress(t, "tcp"), Console: offlineFreeAddress(t, "tcp"), ProxyProtocolSecret: string(secret)}
	source := &cluster.StaticSource{Endpoints: []cluster.Endpoint{{Name: r.cfg.Node.Name, Address: r.cfg.Listeners.DNS[0].Address, InternalAddress: r.cfg.Listeners.Internal.Address, Ready: true, Serving: true}}}
	configDir := filepath.Join(dir, "config")
	start := func(blocks dnsdist.EdgeBlockLister) (*dnsdist.Supervisor, func()) {
		sup, err := dnsdist.New(dnsdist.Config{Options: options, Source: source, Renderer: renderer, ConfigDir: configDir, Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, Blocks: blocks, BlockPoll: 200 * time.Millisecond, StatusPoll: 100 * time.Millisecond, Debounce: 100 * time.Millisecond, StopTimeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- sup.Run(ctx) }()
		stop := func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("supervisor shutdown failed")
				}
			case <-time.After(20 * time.Second):
				t.Error("supervisor shutdown did not join")
			}
		}
		edgeBlockWait(t, "edge ready", func() bool { ok, _ := sup.Ready(); return ok })
		return sup, stop
	}
	// answered reports whether a query from source got the upstream answer;
	// false is a dropped query (timeout), never a refused one.
	answered := func(source string) bool {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(strconv.FormatInt(time.Now().UnixNano(), 36)+".block.example.", dns.TypeA)
		c := &dns.Client{Timeout: 700 * time.Millisecond, Dialer: &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP(source)}}}
		m, _, err := c.ExchangeContext(t.Context(), q, edgeAddr)
		if err != nil {
			return false
		}
		if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
			t.Fatalf("query from %s was answered with rcode %d instead of dropped", source, m.Rcode)
		}
		return true
	}
	installed := func(sup *dnsdist.Supervisor) bool {
		w := httptest.NewRecorder()
		sup.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
		var body struct {
			EdgeBlocks struct {
				Active    int  `json:"active"`
				Installed bool `json:"installed"`
			} `json:"edgeBlocks"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body.EdgeBlocks.Installed
	}

	sup, stop := start(r.internal)
	edgeBlockWait(t, "clients answered before any block", func() bool { return answered("127.0.0.2") && answered("127.0.0.3") && answered("127.0.0.4") })
	var short, long api.EdgeBlock
	r.command(t, http.MethodPost, "/edge/blocks", map[string]any{"prefix": "127.0.0.2", "seconds": edgeblock.MinDuration / time.Second}, http.StatusCreated, &short)
	r.command(t, http.MethodPost, "/edge/blocks", map[string]any{"prefix": "127.0.0.4/32", "seconds": 600}, http.StatusCreated, &long)
	edgeBlockWait(t, "pushed blocks drop both clients", func() bool { return installed(sup) && !answered("127.0.0.2") && !answered("127.0.0.4") })
	if !answered("127.0.0.3") {
		t.Fatal("an unblocked client was dropped")
	}
	t.Log("PASS pushed blocks drop the blocked clients' queries at the real edge; another client is answered")

	r.command(t, http.MethodDelete, "/edge/blocks/"+long.ID, nil, http.StatusOK, nil)
	edgeBlockWait(t, "revoked client answered", func() bool { return answered("127.0.0.4") })
	if answered("127.0.0.2") {
		t.Fatal("revoke of one block lifted another")
	}
	t.Log("PASS revoke lifts only the revoked block")

	// Edge restart with every backend unreachable: the persisted set is
	// reapplied to the new dnsdist and enforced until its expiry.
	stop()
	sup, stop = start(failingBlocks{})
	defer stop()
	edgeBlockWait(t, "persisted block reapplied after the restart", func() bool { return installed(sup) })
	if answered("127.0.0.2") {
		t.Fatal("the restarted edge forgot an unexpired block")
	}
	if !answered("127.0.0.4") || !answered("127.0.0.3") {
		t.Fatal("the restarted edge drops a client it must answer")
	}
	t.Log("PASS restarted edge without any backend keeps the unexpired block")
	if until := time.Until(short.Until); until > 2*time.Second {
		time.Sleep(until - 2*time.Second)
		if answered("127.0.0.2") {
			t.Fatal("block lifted before its expiry")
		}
	}
	deadline := short.Until.Add(5 * time.Second)
	for !answered("127.0.0.2") {
		if time.Now().After(deadline) {
			t.Fatal("block was not lifted within five seconds of its expiry")
		}
	}
	t.Logf("PASS block expired on the edge %.1fs after its expiry", time.Since(short.Until).Seconds())
}

// TestRunServeKubeEdgeBlocksTwoReplicas: on the Kubernetes driver a block
// committed through one replica's API is served to edges by the other
// replica's internal listener, a revoke through the second replica reaches
// the first, and a bearer without a grant is refused.
func TestRunServeKubeEdgeBlocksTwoReplicas(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal("edge block control plane startup", err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var ns corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		t.Fatal(err)
	}
	// The fixture kubeconfig carries no credentials; an authenticating proxy
	// stands in for the in-cluster ServiceAccount, as in the blocking test.
	target, err := url.Parse(restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(restConfig); err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	configs := []*config.Config{offlineServeConfig(t, gate.URL), offlineServeConfig(t, gate.URL)}
	for i, cfg := range configs {
		cfg.Node.Name = []string{"edge-a", "edge-b"}[i]
		cfg.Store.Kube.ExpectedClusterID = string(ns.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	}
	wire, _ := tokens.Generate()
	t.Setenv(tokens.EnvBootstrapToken, wire)
	t.Setenv(tokens.EnvBootstrapTokenFile, "")
	for _, cfg := range configs {
		_ = startStaticMount(t, cfg)
	}
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: make([]byte, 32), NodeName: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	internal := internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 3 * time.Second})
	served := func(i int) (internalapi.EdgeBlockList, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		return internal.EdgeBlocks(ctx, configs[i].Listeners.Internal.Address)
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	request := func(i int, method, path, token string, body any, out any) int {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		r, err := http.NewRequestWithContext(t.Context(), method, "http://"+configs[i].Listeners.API.Address+api.BasePath+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		response, err := hc.Do(r)
		if err != nil {
			t.Fatal("edge block API exchange", err)
		}
		defer func() { _ = response.Body.Close() }()
		if out != nil && response.StatusCode < 300 {
			if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode
	}
	edgeBlockWait(t, "both replicas serve the empty set", func() bool {
		a, errA := served(0)
		b, errB := served(1)
		return errA == nil && errB == nil && len(a.Blocks) == 0 && len(b.Blocks) == 0
	})
	if code := request(0, http.MethodPost, "/edge/blocks", "kdr_not_a_token", map[string]any{"prefix": "192.0.2.0/24", "seconds": 600}, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create = %d", code)
	}
	var created api.EdgeBlock
	if code := request(0, http.MethodPost, "/edge/blocks", wire, map[string]any{"prefix": "192.0.2.0/24", "seconds": 600, "reason": "flood"}, &created); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	var owner string
	edgeBlockWait(t, "second replica serves the block", func() bool {
		l, err := served(1)
		owner = l.Owner
		return err == nil && l.Revision == 1 && len(l.Blocks) == 1 && l.Blocks[0].ID == created.ID
	})
	if !strings.HasPrefix(owner, "kubernetes:") {
		t.Fatalf("owner %q", owner)
	}
	if code := request(1, http.MethodPost, "/edge/blocks", wire, map[string]any{"prefix": "192.0.2.9/24", "seconds": 600}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate through the other replica = %d", code)
	}
	if code := request(1, http.MethodDelete, "/edge/blocks/"+created.ID, wire, nil, nil); code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}
	edgeBlockWait(t, "first replica serves the revoke", func() bool {
		l, err := served(0)
		return err == nil && l.Revision == 2 && len(l.Blocks) == 0 && l.Owner == owner
	})
}
