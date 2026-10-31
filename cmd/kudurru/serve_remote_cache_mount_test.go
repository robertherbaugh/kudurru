package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
)

type remoteCachePoll struct {
	good, bad           *internalapi.Client
	unauthorized, stall atomic.Bool
}

func (p *remoteCachePoll) Zones(ctx context.Context, addr string) (internalapi.ZoneList, error) {
	if p.stall.Load() {
		<-ctx.Done()
		return internalapi.ZoneList{}, ctx.Err()
	}
	c := p.good
	if p.unauthorized.Load() {
		c = p.bad
	}
	return c.Zones(ctx, addr)
}

// This opt-in case runs the real root, SQLite publications, authenticated
// internal listener and generated supervisor against the pinned external daemon.
func TestRunServeRemoteEdgeRegexAndLateFill(t *testing.T) {
	if os.Getenv("KUDURRU_F39_LIVE") != "1" {
		t.Skip("set KUDURRU_F39_LIVE=1 for owned pinned dnsdist acceptance")
	}
	parent := os.Getenv("KUDURRU_F39_WORKDIR")
	if parent == "" {
		t.Fatal("owned scratch parent required")
	}
	dir, err := os.MkdirTemp(parent, "remote-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) }) //nolint:gosec // only this fresh owned fixture
	t.Setenv("TMPDIR", dir)
	name := "kudurru-f39-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	docker := []string{"--host", "unix:///tmp/kudurru-lab-docker.sock"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", append(docker, "rm", "--force", name)...).Run() //nolint:gosec // fixed daemon, unique owned name; no shell or arbitrary command
	})
	image := "sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e"
	fixture, _ := json.Marshal(map[string]string{"name": name, "image": image})
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0o600); err != nil { //nolint:gosec // fresh private temporary child below explicit fixture parent
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
	var calls atomic.Int64
	var hold atomic.Bool
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	upstream := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		calls.Add(1)
		if q.Question[0].Name == "held.regex.example." && hold.CompareAndSwap(true, false) {
			entered <- struct{}{}
			select {
			case <-resume:
			case <-time.After(10 * time.Second):
			}
		}
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 80)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	t.Cleanup(func() { release(); _ = upstream.Shutdown() })
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	cfg.Blocking.Enabled = true
	cfg.Blocking.DefaultResponse = "customIP"
	cfg.Blocking.CustomIP.V4 = "192.0.2.99"
	cfg.Blocking.BlockTTL = config.Duration(300 * time.Second)
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(9 * time.Second)}}
	secret := bytes.Repeat([]byte{0x39}, 32)
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(dir, "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
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
	wait := func(label string, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatal("timed out: " + label)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	wait("root ready", func() bool { return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK })
	// Login can publish an independent session owner. Complete its expensive
	// password work before warming the cache or holding an upstream request.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32), NodeName: "remote-edge"})
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(dir, "wrong-token")
	if err := os.WriteFile(wrong, []byte("wrong-internal-fixture-token"), 0o600); err != nil { //nolint:gosec // fresh owned negative credential fixture, no operator path write
		t.Fatal(err)
	}
	bad, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32), NodeName: "remote-edge", TokenFile: wrong})
	if err != nil {
		t.Fatal(err)
	}
	poller := &remoteCachePoll{good: internalapi.NewClient(creds, internalapi.ClientOptions{}), bad: internalapi.NewClient(bad, internalapi.ClientOptions{})}
	_, port, _ := net.SplitHostPort(offlineFreeAddress(t, "tcp"))
	listenPort, _ := strconv.Atoi(port)
	sup, err := dnsdist.New(dnsdist.Config{Options: edge.Options{ServerID: "remote-cache-edge", Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: listenPort}, API: offlineFreeAddress(t, "tcp"), Console: offlineFreeAddress(t, "tcp"), ProxyProtocolSecret: string(secret), ResolverCacheSize: 1000}, Source: &cluster.StaticSource{Endpoints: []cluster.Endpoint{{Name: cfg.Node.Name, Address: cfg.Listeners.DNS[0].Address, InternalAddress: cfg.Listeners.Internal.Address, Ready: true, Serving: true}}}, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, Zones: poller, ZonePoll: 100 * time.Millisecond, StatusPoll: 100 * time.Millisecond, Debounce: 100 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	edgeCtx, edgeCancel := context.WithCancel(ctx)
	edgeDone := make(chan error, 1)
	go func() { edgeDone <- sup.Run(edgeCtx) }()
	t.Cleanup(func() {
		edgeCancel()
		select {
		case err := <-edgeDone:
			if err != nil {
				t.Error("supervisor shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("supervisor shutdown did not join")
		}
	})
	status := func() (string, string) {
		w := httptest.NewRecorder()
		sup.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
		var body struct {
			Mode     string `json:"resolverCacheMode"`
			Identity string `json:"resolverCacheIdentity"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body.Mode, body.Identity
	}
	wait("authenticated cache qualification", func() bool { mode, _ := status(); ready, _ := sup.Ready(); return mode == "qualified" && ready })
	pid := func() string {
		out, err := exec.CommandContext(t.Context(), "docker", append(docker, "inspect", "--format", "{{.State.Pid}}", name)...).Output() //nolint:gosec // fixed daemon, generated owned container name, fixed read-only format
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	originalPID := pid()
	if originalPID == "" || originalPID == "0" {
		t.Fatal("missing actual daemon PID")
	}
	address := net.JoinHostPort("127.0.0.1", port)
	exchange := func(name string) (*dns.Msg, error) {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		answer, _, err := (&dns.Client{Timeout: 10 * time.Second}).ExchangeContext(t.Context(), q, address)
		return answer, err
	}
	want := func(name, ip string) {
		t.Helper()
		m, err := exchange(name)
		if err != nil || m == nil || len(m.Answer) != 1 {
			t.Fatal("edge query failed")
		}
		a, ok := m.Answer[0].(*dns.A)
		if !ok || a.A.String() != ip {
			t.Fatal("edge answer retained stale publication")
		}
	}
	answers := func(ip string) bool {
		m, err := exchange("probe.example.")
		if err != nil || m == nil || len(m.Answer) != 1 {
			return false
		}
		a, ok := m.Answer[0].(*dns.A)
		return ok && a.A.String() == ip
	}
	want("probe.example.", "192.0.2.80")
	before := calls.Load()
	want("probe.example.", "192.0.2.80")
	if calls.Load() != before {
		t.Fatal("real resolver packet cache did not warm")
	}
	hold.Store(true)
	heldDone := make(chan *dns.Msg, 1)
	go func() { m, _ := exchange("held.regex.example."); heldDone <- m }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("original query did not reach upstream barrier")
	}
	_, oldKey := status()
	var deny api.DomainWriteResult
	controlMountedRequest(t, client, base, http.MethodPost, "/denylist", login.CSRF, api.DomainEntriesCreate{Type: "regex", Values: []string{`^(probe|held\.regex)\.example$`}}, http.StatusOK, &deny)
	wait("regex publication invalidation", func() bool {
		mode, key := status()
		return mode == "qualified" && key != "" && key != oldKey && answers("192.0.2.99")
	})
	want("probe.example.", "192.0.2.99")
	release()
	select {
	case m := <-heldDone:
		if m == nil || len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "192.0.2.80" {
			t.Fatal("held original query lost original cut")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held response did not complete")
	}
	want("held.regex.example.", "192.0.2.99")
	t.Log("PASS real publication/regex invalidation; held original response cannot refill replacement cache; same PID")
	if len(deny.Items) != 1 {
		t.Fatal("missing exact regex mutation receipt")
	}
	_, oldKey = status()
	controlMountedRequest(t, client, base, http.MethodDelete, "/denylist/"+deny.Items[0].ID, login.CSRF, nil, http.StatusNoContent, nil)
	wait("regex removal invalidation", func() bool {
		mode, key := status()
		return mode == "qualified" && key != "" && key != oldKey && answers("192.0.2.80")
	})
	want("probe.example.", "192.0.2.80")
	// Re-add then disable temporarily. Stall the next pull: lease expiry must
	// detach independently at the captured timer rather than extend stale proof.
	controlMountedRequest(t, client, base, http.MethodPost, "/denylist", login.CSRF, api.DomainEntriesCreate{Type: "regex", Values: []string{`^(probe|held\.regex)\.example$`}}, http.StatusOK, &deny)
	wait("regex restored", func() bool {
		m, e := exchange("probe.example.")
		return e == nil && len(m.Answer) == 1 && m.Answer[0].(*dns.A).A.String() == "192.0.2.99"
	})
	// An allow regex must override the already-warm deny, and its removal
	// must restore that original deny without reusing the former cache.
	_, oldKey = status()
	var allow api.DomainWriteResult
	controlMountedRequest(t, client, base, http.MethodPost, "/allowlist", login.CSRF, api.DomainEntriesCreate{Type: "regex", Values: []string{`^(probe|held\.regex)\.example$`}}, http.StatusOK, &allow)
	wait("regex allow override invalidation", func() bool {
		mode, key := status()
		return mode == "qualified" && key != "" && key != oldKey && answers("192.0.2.80")
	})
	want("probe.example.", "192.0.2.80")
	if len(allow.Items) != 1 {
		t.Fatal("missing exact regex allow receipt")
	}
	_, oldKey = status()
	controlMountedRequest(t, client, base, http.MethodDelete, "/allowlist/"+allow.Items[0].ID, login.CSRF, nil, http.StatusNoContent, nil)
	wait("regex allow removal restores deny", func() bool {
		mode, key := status()
		return mode == "qualified" && key != "" && key != oldKey && answers("192.0.2.99")
	})
	want("probe.example.", "192.0.2.99")
	_, oldKey = status()
	var blocking api.BlockingState
	// Leave room for the original bounded pull and external console command
	// under race instrumentation; this changes no production timeout.
	controlMountedRequest(t, client, base, http.MethodPost, "/dns/blocking", login.CSRF, map[string]any{"blocking": false, "timer": 8}, http.StatusOK, &blocking)
	wait("disabled publication", func() bool {
		mode, key := status()
		return mode == "qualified" && key != "" && key != oldKey && answers("192.0.2.80")
	})
	want("probe.example.", "192.0.2.80")
	if blocking.Until == nil {
		t.Fatal("missing original timer receipt")
	}
	proofCtx, proofCancel := context.WithTimeout(t.Context(), 3*time.Second)
	proof, proofErr := poller.good.Zones(proofCtx, cfg.Listeners.Internal.Address)
	proofCancel()
	if proofErr != nil || proof.Resolver == nil || proof.Resolver.BlockingEnabled || !proof.Resolver.DisabledUntil.Equal(*blocking.Until) || !proof.Resolver.ValidUntil.Equal(*blocking.Until) {
		t.Fatal("remote cache observation lost the original absolute timer boundary")
	}
	poller.stall.Store(true)
	wait("independent exact-boundary cache bypass", func() bool { mode, _ := status(); return strings.HasPrefix(mode, "bypass-") })
	wait("backend original timer expiry", func() bool { return !time.Now().Before(*blocking.Until) })
	want("probe.example.", "192.0.2.99")
	poller.stall.Store(false)
	wait("fresh proof after timer", func() bool { mode, _ := status(); return mode == "qualified" })
	poller.unauthorized.Store(true)
	wait("original internal auth refusal bypass", func() bool { mode, _ := status(); return mode == "bypass-unavailable" })
	want("probe.example.", "192.0.2.99")
	poller.unauthorized.Store(false)
	wait("authenticated reconnect", func() bool { mode, _ := status(); return mode == "qualified" })
	if pid() != originalPID {
		t.Fatal("resolver invalidation restarted actual daemon")
	}
	t.Logf("PASS regex allow/deny removal/ABA, absolute blocking expiry during stalled pull, actual token refusal/reconnect, PID preserved; backend queries=%d", calls.Load())
	// The wrapper loses ONLY the response, after the real daemon executed the
	// next cache mutation. Production must retire this uncertain child rather
	// than keep its unacknowledged cache serving while retries continue.
	if err := os.WriteFile(filepath.Join(dir, "lose-ack-none"), nil, 0o600); err != nil { //nolint:gosec // fixed fault file in this fresh private fixture
		t.Fatal(err)
	}
	controlMountedRequest(t, client, base, http.MethodDelete, "/denylist/"+deny.Items[0].ID, login.CSRF, nil, http.StatusNoContent, nil)
	wait("lost actual cache acknowledgment", func() bool { mode, _ := status(); return mode == "uncertain" })
	if ready, _ := sup.Ready(); ready {
		t.Fatal("uncertain actual cache mutation reported ready")
	}
	wait("uncached restart and fresh proof after lost acknowledgment", func() bool {
		mode, _ := status()
		ready, _ := sup.Ready()
		return mode == "qualified" && ready && pid() != originalPID && answers("192.0.2.80")
	})
	want("probe.example.", "192.0.2.80")
	t.Log("PASS lost real console acknowledgment retired its original child and requalified accepted uncached restart; no DNS replay")
}
