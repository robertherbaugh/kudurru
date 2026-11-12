package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// TestRunServeTracingLiveDnsdist runs the pinned dnsdist 2.1.3 image in
// front of a real runServe (D382): dnsdist traces the query, sends PROXYv2
// with the shared-secret TLV and its TRACEPARENT, and kudurru's exported
// trace continues dnsdist's trace id with dnsdist's span as the parent. The
// query row carries the same id; the public upstream never sees the option.
func TestRunServeTracingLiveDnsdist(t *testing.T) {
	if os.Getenv("KUDURRU_TRACING_LIVE") != "1" {
		t.Skip("set KUDURRU_TRACING_LIVE=1 and KUDURRU_TRACING_LIVE_WORKDIR for the pinned dnsdist TRACEPARENT acceptance")
	}
	parent := os.Getenv("KUDURRU_TRACING_LIVE_WORKDIR")
	if !filepath.IsAbs(parent) {
		t.Fatal("an absolute scratch parent the Docker daemon can read is required")
	}
	public := startOptionRecorder(t)
	coll := &traceCollector{spans: map[string][]*tracepb.Span{}}
	otlpServer := httptest.NewServer(coll)
	defer otlpServer.Close()

	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Exclude.SUDN = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"public=" + public.addr}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	secret := bytes.Repeat([]byte{0x51}, 40)
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Tracing.Enabled = true
	cfg.Tracing.Endpoint = otlpServer.URL + "/v1/traces"
	cfg.Tracing.SampleRate, cfg.Tracing.SlowThreshold = 0, 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	stop := startStaticMount(t, cfg)
	defer stop()

	dir, err := os.MkdirTemp(parent, "obs-tracing-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()     //nolint:gosec // the directory MkdirTemp just made
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the daemon reads the config as another user
		t.Fatal(err)
	}
	listen := offlineFreeAddress(t, "udp")
	lua := fmt.Sprintf(`setLocal(%q)
newServer({address=%q, useProxyProtocol=true, healthCheckMode="up"})
addAction(AllRule(), SetAdditionalProxyProtocolValueAction(224, %q))
setOpenTelemetryTracing(true)
addAction(AllRule(), SetTraceAction(true, {sendDownstreamTraceparent=true}))
`, listen, cfg.Listeners.DNS[0].Address, string(secret))
	if err := os.WriteFile(filepath.Join(dir, "dnsdist.conf"), []byte(lua), 0o644); err != nil { //nolint:gosec // read by the daemon's user; the secret is a fixture
		t.Fatal(err)
	}
	name := "obs-tracing-live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const image = "sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e"
	docker := func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append([]string{"--host", "unix:///tmp/kudurru-lab-docker.sock"}, args...)...).CombinedOutput() //nolint:gosec // fixed test daemon, owned fixture
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = docker(ctx, "rm", "--force", name)
	})
	if out, err := docker(t.Context(), "run", "-d", "--name", name, "--network", "host", "-v", dir+":/cfg:ro", "--entrypoint", "dnsdist", image, "--supervised", "--disable-syslog", "-C", "/cfg/dnsdist.conf"); err != nil {
		t.Fatalf("start dnsdist: %v %s", err, out)
	}

	q := new(dns.Msg)
	q.SetQuestion("live.public.example.", dns.TypeA)
	var answer *dns.Msg
	deadline := time.Now().Add(20 * time.Second)
	for {
		answer, _, err = (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(q, listen)
		if err == nil && answer != nil && answer.Rcode == dns.RcodeSuccess && len(answer.Answer) == 1 {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := docker(context.Background(), "logs", name)
			t.Fatalf("no answer through dnsdist: %v %v\n%s", err, answer, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if data, hits := public.option("live.public.example."); hits < 1 || data != nil {
		t.Fatalf("public upstream hits %d option %x", hits, data)
	}
	var traceID string
	var root *tracepb.Span
	until := time.Now().Add(15 * time.Second)
	for root == nil {
		if time.Now().After(until) {
			t.Fatal("no trace exported for the dnsdist query")
		}
		time.Sleep(50 * time.Millisecond)
		coll.mu.Lock()
		for id, spans := range coll.spans {
			for _, s := range spans {
				if s.Name == tracing.RootName && len(s.ParentSpanId) == 8 && !bytes.Equal(s.ParentSpanId, make([]byte, 8)) {
					traceID, root = id, s
				}
			}
		}
		coll.mu.Unlock()
	}
	t.Logf("dnsdist trace %s, dnsdist span %s, kudurru root %s", traceID, hex.EncodeToString(root.ParentSpanId), hex.EncodeToString(root.SpanId))

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	found := false
	until = time.Now().Add(15 * time.Second)
	for !found && time.Now().Before(until) {
		var page api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit=20", "", nil, http.StatusOK, &page)
		for _, r := range page.Queries {
			if r.Domain == "live.public.example" && r.TraceID == traceID && r.Flags.Traceparent {
				found = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("the query row does not carry dnsdist's trace id")
	}
}
