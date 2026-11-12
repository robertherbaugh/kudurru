package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	runServeRemoteEdgeCache(t, false)
}

func TestRunServeRemoteEdgeAuthoritativeCacheACL(t *testing.T) {
	runServeRemoteEdgeCache(t, true)
}

func runServeRemoteEdgeCache(t *testing.T, authoritativeOnly bool) {
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
	image := kudurrutest.DnsdistImage
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
	// Exercise the original license gate with a signed throwaway test issuer,
	// just like the mounted browser fixture. Community cannot add policies.
	licenseKey := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureEdge}})
	cfg.Auth.IdentityProviders = nil
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	// This positive cache fixture has no per-client backend verdicts. The
	// default per-prefix limiter correctly disqualifies a shared packet cache.
	cfg.RateLimit.Enabled = false
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
	sup, err := dnsdist.New(dnsdist.Config{Options: edge.Options{ServerID: "remote-cache-edge", Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: listenPort}, API: offlineFreeAddress(t, "tcp"), Console: offlineFreeAddress(t, "tcp"), ProxyProtocolSecret: string(secret), ResolverCacheSize: 1000, AuthoritativeCache: true, AuthoritativeZones: []string{"private.example."}}, Source: &cluster.StaticSource{Endpoints: []cluster.Endpoint{{Name: cfg.Node.Name, Address: cfg.Listeners.DNS[0].Address, InternalAddress: cfg.Listeners.Internal.Address, Ready: true, Serving: true}}}, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, Zones: poller, ZonePoll: 100 * time.Millisecond, StatusPoll: 100 * time.Millisecond, Debounce: 100 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	authoritativeStatus := func() (string, string, float64, float64) {
		w := httptest.NewRecorder()
		sup.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
		var body struct {
			Mode     string                `json:"authoritativeCacheMode"`
			Identity string                `json:"authoritativeCacheIdentity"`
			DNSDist  *dnsdist.ServerStatus `json:"dnsdist"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		var hits, size float64
		if body.DNSDist != nil {
			for _, pool := range body.DNSDist.Pools {
				if pool.Name == "authoritative" {
					hits, size = pool.CacheHits, pool.CacheSize
				}
			}
		}
		return body.Mode, body.Identity, hits, size
	}
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
	writePublished := func(method, path string, body any, want int) {
		t.Helper()
		read := func() (uint64, bool) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			z, err := poller.good.Zones(ctx, cfg.Listeners.Internal.Address)
			if err != nil || z.Resolver == nil {
				return 0, false
			}
			return z.Resolver.Publication, true
		}
		var before uint64
		var unchangedSince time.Time
		wait("quiet original publication before one-shot mutation", func() bool {
			publication, ok := read()
			if !ok {
				unchangedSince = time.Time{}
				return false
			}
			if publication != before || unchangedSince.IsZero() {
				before, unchangedSince = publication, time.Now()
				return false
			}
			// Initial controller/primary status work is independent of DNS
			// readiness. Observe quiescence rather than replay a refused write.
			return time.Since(unchangedSince) >= 500*time.Millisecond
		})
		controlMountedRequest(t, client, base, method, path, login.CSRF, body, want, nil)
		// One API write, no mutation replay. A successful original complete
		// observation proves the durable source has caught up before another
		// original-authorized command is submitted.
		wait("complete publication after mutation", func() bool { after, ok := read(); return ok && after > before })
	}
	// Distinct source addresses are carried by the actual authenticated
	// PROXYv2 listener. Identical questions must never share client verdicts.
	from := func(source, qname string) (*dns.Msg, error) {
		q := new(dns.Msg)
		q.SetQuestion(qname, dns.TypeA)
		c := &dns.Client{Timeout: 5 * time.Second, Dialer: &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP(source)}}}
		m, _, err := c.ExchangeContext(t.Context(), q, address)
		return m, err
	}
	clientAnswer := func(source, qname, ip string, rcode int) {
		t.Helper()
		m, err := from(source, qname)
		if err != nil || m == nil || m.Rcode != rcode {
			t.Fatal("original client response failed", source, qname)
		}
		if ip == "" {
			if len(m.Answer) != 0 {
				t.Fatal("denied original client received cached answer")
			}
			return
		}
		if len(m.Answer) != 1 {
			t.Fatal("original client missing answer")
		}
		a, ok := m.Answer[0].(*dns.A)
		if !ok || a.A.String() != ip {
			t.Fatal("shared packet cache leaked another client's verdict", source)
		}
	}
	if !authoritativeOnly {
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
		qualifiedPID := pid()
		for _, source := range []string{"127.0.0.2", "127.0.0.3"} {
			clientAnswer(source, "probe.example.", "192.0.2.80", dns.RcodeSuccess)
		}
		group := &public.ClientGroup{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "ClientGroup"}, ObjectMeta: metav1.ObjectMeta{Name: "cache-scoped", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.2"}}}}
		policy := &public.Policy{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "Policy"}, ObjectMeta: metav1.ObjectMeta{Name: "cache-scoped", Namespace: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"cache-scoped"}, Priority: 1000, Rewrites: []public.Rewrite{{Name: "probe.example", Targets: []string{"192.0.2.99"}}}}}
		writePublished(http.MethodPost, "/objects/clientgroups", group, http.StatusCreated)
		writePublished(http.MethodPost, "/objects/policies", policy, http.StatusCreated)
		wait("client policy shared-cache bypass", func() bool { mode, _ := status(); return mode == "bypass-client-scope" })
		for range 3 {
			clientAnswer("127.0.0.2", "probe.example.", "192.0.2.99", dns.RcodeSuccess)
			clientAnswer("127.0.0.3", "probe.example.", "192.0.2.80", dns.RcodeSuccess)
			clientAnswer("127.0.0.3", "probe.example.", "192.0.2.80", dns.RcodeSuccess)
			clientAnswer("127.0.0.2", "probe.example.", "192.0.2.99", dns.RcodeSuccess)
		}
		writePublished(http.MethodDelete, "/objects/policies/default/cache-scoped", nil, http.StatusNoContent)
		writePublished(http.MethodDelete, "/objects/clientgroups/default/cache-scoped", nil, http.StatusNoContent)
		wait("uniform cache restored after client policy", func() bool { mode, _ := status(); return mode == "qualified" })
		t.Log("PASS warm uniform cache then two original PROXYv2 clients alternate distinct policy verdicts under acknowledged client-scope bypass")
		var defaultPolicy public.Policy
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/policies/default/default", "", nil, http.StatusOK, &defaultPolicy)
		originalPolicy := defaultPolicy.DeepCopy()
		list := &public.BlockList{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "BlockList"}, ObjectMeta: metav1.ObjectMeta{Name: "cache-client-rule", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "inline", Format: "adblock", Inline: []string{"||probe.example^$client=127.0.0.2"}}}}}
		writePublished(http.MethodPost, "/objects/blocklists", list, http.StatusCreated)
		defaultPolicy.Spec.BlockListRefs = append(defaultPolicy.Spec.BlockListRefs, "cache-client-rule")
		writePublished(http.MethodPut, "/objects/policies/default/default", &defaultPolicy, http.StatusOK)
		wait("compiled client-rule shared-cache bypass", func() bool { mode, _ := status(); return mode == "bypass-rule-client-scope" })
		for range 3 {
			clientAnswer("127.0.0.3", "probe.example.", "192.0.2.80", dns.RcodeSuccess)
			clientAnswer("127.0.0.2", "probe.example.", "192.0.2.99", dns.RcodeSuccess)
			clientAnswer("127.0.0.2", "probe.example.", "192.0.2.99", dns.RcodeSuccess)
			clientAnswer("127.0.0.3", "probe.example.", "192.0.2.80", dns.RcodeSuccess)
		}
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/policies/default/default", "", nil, http.StatusOK, &defaultPolicy)
		defaultPolicy.Spec = originalPolicy.Spec
		writePublished(http.MethodPut, "/objects/policies/default/default", &defaultPolicy, http.StatusOK)
		writePublished(http.MethodDelete, "/objects/blocklists/default/cache-client-rule", nil, http.StatusNoContent)
		wait("uniform cache restored after compiled client rule", func() bool { mode, _ := status(); return mode == "qualified" })
		t.Log("PASS compiled $client rule with no scoped ClientGroup preserves distinct same-question verdicts; original uniform policy restored")
		forward := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "cache-forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.example.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
		writePublished(http.MethodPost, "/objects/dnszones", forward, http.StatusCreated)
		wait("uniform forward zone qualified", func() bool {
			mode, _ := status()
			m, err := from("127.0.0.2", "probe.forward.example.")
			return mode == "qualified" && err == nil && m != nil && len(m.Answer) == 1
		})
		clientAnswer("127.0.0.2", "probe.forward.example.", "192.0.2.80", dns.RcodeSuccess)
		before = calls.Load()
		clientAnswer("127.0.0.3", "probe.forward.example.", "192.0.2.80", dns.RcodeSuccess)
		if calls.Load() != before {
			t.Fatal("uniform forward-zone response did not warm shared resolver cache")
		}
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/cache-forward", "", nil, http.StatusOK, forward)
		forward.Spec.Policy.QueryAccess = public.AccessUseNetworkACL
		forward.Spec.Policy.QueryACL = []string{"127.0.0.2/32"}
		writePublished(http.MethodPut, "/objects/dnszones/cache-forward", forward, http.StatusOK)
		wait("forward-zone query ACL shared-cache bypass", func() bool { mode, _ := status(); return mode == "bypass-zone-client-acl" })
		for range 3 {
			clientAnswer("127.0.0.2", "probe.forward.example.", "192.0.2.80", dns.RcodeSuccess)
			clientAnswer("127.0.0.3", "probe.forward.example.", "", dns.RcodeRefused)
			clientAnswer("127.0.0.3", "probe.forward.example.", "", dns.RcodeRefused)
			clientAnswer("127.0.0.2", "probe.forward.example.", "192.0.2.80", dns.RcodeSuccess)
		}
		if pid() != qualifiedPID {
			t.Fatal("client-isolation bypass restarted actual daemon")
		}
		t.Log("PASS warm allowed forward-zone answer then ACL-denied original client REFUSED, alternating both orders; same daemon PID; resolver-path ACL isolation qualified")
		return
	}
	awaitPrimarySource := func() {
		// Unlike the settings fixture's already-started primary, this fixture
		// creates a new zone and its data. Use the existing bounded observation
		// budget for admission, with the parent's exact original source proof.
		dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
		wait("new primary original source and readiness aligned", func() bool {
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal("primary source observation open failed")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var pending, notify, heads, unavailable, unaligned int
			err = db.QueryRowContext(ctx, controlMountedSourceQuery).Scan(&pending, &notify, &heads, &unavailable, &unaligned)
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatal("primary source observation read/close failed")
			}
			return pending == 0 && notify == 0 && heads == 1 && unavailable == 0 && unaligned == 0
		})
	}
	primary := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "cache-primary"}, Spec: public.DNSZoneSpec{ZoneName: "private.example.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	writePublished(http.MethodPost, "/objects/dnszones", primary, http.StatusCreated)
	records := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "cache-primary", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "cache-primary"}, Endpoints: []public.Endpoint{{DNSName: "probe.private.example.", RecordType: "A", Targets: []string{"192.0.2.81"}, RecordTTL: 300}}}}
	writePublished(http.MethodPost, "/objects/dnsrecordsets", records, http.StatusCreated)
	awaitPrimarySource()
	// The fixed configured route already selects the authoritative pool. Its
	// measured cache hits below prove the actual route, rather than inferred
	// dynamic-zone status (which need not change an identical fixed route).
	wait("original static primary authoritative cache qualified", func() bool {
		mode, _, _, size := authoritativeStatus()
		m, err := from("127.0.0.2", "probe.private.example.")
		return mode == "qualified" && size > 0 && err == nil && m != nil && m.Authoritative && len(m.Answer) == 1 && m.Answer[0].(*dns.A).A.String() == "192.0.2.81"
	})
	_, _, hits, _ := authoritativeStatus()
	clientAnswer("127.0.0.2", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
	clientAnswer("127.0.0.3", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
	wait("real authoritative packet cache hit", func() bool {
		clientAnswer("127.0.0.2", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
		clientAnswer("127.0.0.3", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
		_, _, got, _ := authoritativeStatus()
		return got > hits
	})
	authPID := pid() // topology changes can use the existing validated restart
	t.Log("PASS measured real authoritative pool cache hit across two original PROXYv2 clients")
	readPrimary := func() {
		// Warm DNS and cache eligibility do not imply that independent primary
		// status/notification work has settled. Observe those original durable
		// receipts before the fixture's one-shot source mutation.
		awaitPrimarySource()
		// A new independent read may observe a transient pending publication.
		// Bound read-only polling; never retry an original mutation or repair
		// its failed serving lease inside the API under test.
		wait("original primary source readable", func() bool {
			response, err := client.Get(base + "/objects/dnszones/cache-primary")
			if err != nil {
				return false
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode == http.StatusServiceUnavailable {
				return false
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("primary read status=%d", response.StatusCode)
			}
			if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(primary); err != nil {
				t.Fatal("original primary read decode failed")
			}
			return true
		})
	}
	readPrimary()
	primary.Spec.Policy.QueryAccess = public.AccessUseNetworkACL
	primary.Spec.Policy.QueryACL = []string{"127.0.0.2/32"}
	writePublished(http.MethodPut, "/objects/dnszones/cache-primary", primary, http.StatusOK)
	wait("original primary ACL detaches authoritative cache", func() bool {
		mode, _, _, size := authoritativeStatus()
		m, err := from("127.0.0.2", "probe.private.example.")
		return mode == "bypass-zone-client-acl" && size == 0 && err == nil && m != nil && m.Authoritative && len(m.Answer) == 1 && m.Answer[0].(*dns.A).A.String() == "192.0.2.81"
	})
	for range 3 {
		clientAnswer("127.0.0.2", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
		clientAnswer("127.0.0.3", "probe.private.example.", "", dns.RcodeRefused)
		clientAnswer("127.0.0.3", "probe.private.example.", "", dns.RcodeRefused)
		clientAnswer("127.0.0.2", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
	}
	readPrimary()
	primary.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
	writePublished(http.MethodPut, "/objects/dnszones/cache-primary", primary, http.StatusOK)
	wait("uniform primary proof restored", func() bool { mode, _, _, _ := authoritativeStatus(); return mode == "qualified" })
	clientAnswer("127.0.0.3", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
	poller.unauthorized.Store(true)
	wait("original auth failure detaches both caches", func() bool {
		mode, _, _, size := authoritativeStatus()
		resolverMode, _ := status()
		return mode == "bypass-unavailable" && size == 0 && resolverMode == "bypass-unavailable"
	})
	clientAnswer("127.0.0.2", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
	poller.unauthorized.Store(false)
	wait("authoritative authenticated reconnect", func() bool { mode, _, _, _ := authoritativeStatus(); return mode == "qualified" })
	poller.stall.Store(true)
	wait("original authoritative lease expires during stalled pull", func() bool {
		mode, _, _, size := authoritativeStatus()
		return strings.HasPrefix(mode, "bypass-") && size == 0
	})
	clientAnswer("127.0.0.3", "probe.private.example.", "192.0.2.81", dns.RcodeSuccess)
	poller.stall.Store(false)
	wait("authoritative fresh proof after expiry", func() bool { mode, _, _, _ := authoritativeStatus(); return mode == "qualified" })
	if pid() != authPID {
		t.Fatal("ordinary authoritative safety changes restarted actual daemon")
	}
	t.Log("PASS actual authoritative pool cache hit across original clients; warm allow then ACL-denied client REFUSED in both orders, original auth/expiry bypass and reconnect; same PID")
}
