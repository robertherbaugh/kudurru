package dnsdist

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

// chanSource is a MembershipSource driven by the test.
type chanSource struct {
	initial []cluster.Endpoint
	ch      chan []cluster.Endpoint
}

func (c *chanSource) Name() string { return "test" }

func (c *chanSource) List(context.Context) ([]cluster.Endpoint, error) { return c.initial, nil }

func (c *chanSource) Watch(ctx context.Context, fn func([]cluster.Endpoint)) error {
	fn(c.initial)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-c.ch:
			fn(e)
		}
	}
}

func ep(name, ip string) cluster.Endpoint {
	return cluster.Endpoint{Name: name, Address: ip + ":5353", InternalAddress: ip + ":8082", Ready: true, Serving: true}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// fakeBinary links the test binary as <tmp>/bin/dnsdist (TestMain turns
// it into the fake).
func fakeBinary(t *testing.T) string {
	t.Helper()
	// The fake reexecutes this race-instrumented test binary. Its default
	// one-second exit grace is not dnsdist work and would exceed the bounded
	// console/health deadlines. Preserve race detection and all other options;
	// only child startup reads this setting (the parent runtime already did).
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "dnsdist")
	if err := os.Symlink(self, p); err != nil {
		t.Fatal(err)
	}
	return p
}

type harness struct {
	t      *testing.T
	sup    *Supervisor
	dir    string
	src    *chanSource
	cancel context.CancelFunc
	done   chan error
	reg    *prometheus.Registry
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	src := &chanSource{initial: []cluster.Endpoint{ep("node-a", "10.0.0.11")}, ch: make(chan []cluster.Endpoint, 4)}
	reg := prometheus.NewRegistry()
	cfg := Config{
		Options:       edge.Options{ServerID: "edge-test", API: freePort(t), Console: "127.0.0.1:5199", Listen: []string{"127.0.0.1"}, Ports: map[edge.Protocol]int{edge.Do53: 5300}},
		Source:        src,
		ConfigDir:     filepath.Join(dir, "etc"),
		Binary:        fakeBinary(t),
		APIKeyOut:     filepath.Join(dir, "run", "dnsdist-api.key"),
		APIKeyGID:     -1,
		Debounce:      50 * time.Millisecond,
		HealthTimeout: 250 * time.Millisecond,
		PurgeDelay:    20 * time.Millisecond,
		StatusPoll:    50 * time.Millisecond,
		ZonePoll:      50 * time.Millisecond,
		StopTimeout:   5 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registerer:    reg,
		Gatherer:      reg,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, sup: sup, dir: cfg.ConfigDir, src: src, cancel: cancel, done: make(chan error, 1), reg: reg}
	go func() { h.done <- sup.Run(ctx) }()
	t.Cleanup(func() { h.stop() })
	return h
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Error("Run did not return")
	}
}

func (h *harness) log() string {
	b, _ := os.ReadFile(filepath.Join(h.dir, "fake.log")) //nolint:gosec // test-owned path
	return string(b)
}

func (h *harness) count(prefix string) int {
	n := 0
	for _, l := range strings.Split(h.log(), "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; fake log:\n%s", what, h.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) ready() bool { ok, _ := h.sup.Ready(); return ok }

func TestSupervisorLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	h.waitFor("ready", h.ready)

	// The per-start API key is published 0640 and matches the config.
	st, err := os.Stat(filepath.Join(filepath.Dir(h.dir), "run", "dnsdist-api.key")) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("api key mode %v, want 0640", st.Mode().Perm())
	}
	key, _ := os.ReadFile(filepath.Join(filepath.Dir(h.dir), "run", "dnsdist-api.key")) //nolint:gosec // test-owned path
	cfg, _ := os.ReadFile(filepath.Join(h.dir, "dnsdist.yml"))                          //nolint:gosec // test-owned path
	if !strings.Contains(string(cfg), "api_key: "+strings.TrimSpace(string(key))) {
		t.Error("published API key does not match the rendered one")
	}
	if fi, _ := os.Stat(filepath.Join(h.dir, "dnsdist.yml")); fi.Mode().Perm() != 0o600 { //nolint:gosec // test-owned path
		t.Errorf("config mode %v, want 0600 (it holds credentials)", fi.Mode().Perm())
	}
	if h.count("check dnsdist.candidate.yml") != 1 || h.count("start") != 1 {
		t.Fatalf("want one check and one start:\n%s", h.log())
	}

	// Membership changes coalesce into a console update without restarting.
	h.src.ch <- []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("node-b", "10.0.0.12")}
	h.src.ch <- []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("node-b", "10.0.0.12"), ep("node-c", "10.0.0.13")}
	h.waitFor("promoted backends", func() bool { return h.count("console promote") == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := h.count("start"); n != 2 {
		t.Errorf("%d starts, want 2 for validated named-member staging", n)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "dnsdist.yml.prev")); err != nil { //nolint:gosec // test-owned path
		t.Errorf("previous config not kept: %v", err)
	}
	h.waitFor("ready again", h.ready)

	// Not-ready endpoints are filtered; the same rendered set is a no-op.
	notReady := ep("node-d", "10.0.0.14")
	notReady.Ready, notReady.Serving = false, false
	h.src.ch <- []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("node-b", "10.0.0.12"), ep("node-c", "10.0.0.13"), notReady}
	time.Sleep(300 * time.Millisecond)
	if n := h.count("start"); n != 2 {
		t.Errorf("%d starts after an unchanged rendered set, want 2", n)
	}

	// Backends down → not ready, metric 0.
	if err := os.WriteFile(filepath.Join(h.dir, "fake-down"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.waitFor("not ready", func() bool { return !h.ready() })
	_, msg := h.sup.Ready()
	if !strings.Contains(msg, "0 of 3") {
		t.Errorf("ready message %q", msg)
	}
	rec := httptest.NewRecorder()
	h.sup.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz/ready", nil))
	if rec.Code != 503 {
		t.Errorf("/healthz/ready = %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.sup.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var status struct {
		Ready   bool         `json:"ready"`
		Message string       `json:"message"`
		DNSDist ServerStatus `json:"dnsdist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Ready || !strings.Contains(status.Message, "0 of 3") || status.DNSDist.UpCount() != 0 {
		t.Fatalf("unready status = %+v", status)
	}
	rec = httptest.NewRecorder()
	h.sup.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `kudurru_edge_backend_up{backend="node-b"} 0`) {
		t.Errorf("metrics:\n%s", rec.Body.String())
	}
	_ = os.Remove(filepath.Join(h.dir, "fake-down"))
	h.waitFor("ready after recovery", h.ready)
	rec = httptest.NewRecorder()
	h.sup.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz/ready", nil))
	if rec.Code != 200 {
		t.Errorf("/healthz/ready = %d, want 200", rec.Code)
	}

	// SIGTERM forwarding on shutdown.
	h.stop()
	if h.count("stop") != 2 {
		t.Errorf("want two clean stops:\n%s", h.log())
	}
}

func TestSupervisorRejectsBadRender(t *testing.T) {
	h := newHarness(t, nil)
	h.waitFor("ready", h.ready)
	// The fake refuses any file containing FAIL-CHECK: the running
	// configuration and process must stay.
	h.src.ch <- []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("FAIL-CHECK", "10.0.0.66")}
	h.waitFor("a failed check", func() bool { return testutilCounter(h.reg, "kudurru_edge_renders_total", "check_failed") >= 1 })
	if n := h.count("start"); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}
	cfg, _ := os.ReadFile(filepath.Join(h.dir, "dnsdist.yml")) //nolint:gosec // test-owned path
	if strings.Contains(string(cfg), "FAIL-CHECK") {
		t.Error("a configuration that failed --check-config was swapped in")
	}
}

func TestSupervisorInitialCheckFailure(t *testing.T) {
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	sup, err := New(Config{
		Options:   edge.Options{API: freePort(t)},
		Source:    &cluster.StaticSource{Endpoints: []cluster.Endpoint{{Name: "FAIL-CHECK", Address: "10.0.0.1:5353", Ready: true}}},
		ConfigDir: dir, Binary: fakeBinary(t), APIKeyGID: -1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Registerer: reg, Gatherer: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "configuration check failed") {
		t.Fatalf("Run = %v, want the check failure", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dnsdist.yml")); !os.IsNotExist(err) { //nolint:gosec // test-owned path
		t.Error("an unchecked configuration was installed")
	}
}

func TestSupervisorChildCrash(t *testing.T) {
	h := newHarness(t, nil)
	h.waitFor("ready", h.ready)
	pid := h.sup.child.Pid()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitFor("restart after a crash", func() bool { return h.count("start") == 2 })
	h.waitFor("ready after the crash", h.ready)
}

// fakeZones is a ZoneLister whose serials the test changes.
type fakeZones struct {
	mu    sync.Mutex
	zones []internalapi.Zone
	fail  bool
}

func (f *fakeZones) set(z ...internalapi.Zone) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones = z
}

func (f *fakeZones) Zones(_ context.Context, addr string) (internalapi.ZoneList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return internalapi.ZoneList{}, internalapi.ErrZonesUnsupported
	}
	return internalapi.ZoneList{Node: addr, Zones: append([]internalapi.Zone(nil), f.zones...)}, nil
}

func TestSupervisorZoneSerialFlush(t *testing.T) {
	zones := &fakeZones{}
	zones.set(internalapi.Zone{Name: "Example.COM", Serial: 1})
	h := newHarness(t, func(c *Config) {
		c.Zones = zones
		c.Options.AuthoritativeCache = true
	})
	h.waitFor("zone routing restart", func() bool {
		cfg, _ := os.ReadFile(filepath.Join(h.dir, "dnsdist.yml")) //nolint:gosec // test-owned path
		return strings.Contains(string(cfg), "- example.com.") && h.count("start") == 2
	})
	h.waitFor("ready", h.ready)
	zones.set(internalapi.Zone{Name: "example.com.", Serial: 2})
	h.waitFor("flush", func() bool {
		return strings.Contains(h.log(), "flush pool=authoritative name=example.com. suffix=true")
	})
	if n := h.count("start"); n != 2 {
		t.Errorf("a serial change restarted dnsdist (%d starts)", n)
	}
}

func TestSupervisorZonesUnsupported(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Zones = &fakeZones{fail: true}
		c.Options.AuthoritativeZones = []string{"lan."}
	})
	h.waitFor("ready", h.ready)
	time.Sleep(200 * time.Millisecond)
	cfg, _ := os.ReadFile(filepath.Join(h.dir, "dnsdist.yml")) //nolint:gosec // test-owned path
	if !strings.Contains(string(cfg), "- lan.") || h.count("start") != 1 {
		t.Errorf("static zones must render and nothing restart:\n%s", h.log())
	}
}

func TestSupervisorLuaRenderer(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Renderer = LuaRenderer{} })
	h.waitFor("ready", h.ready)
	if h.count("check dnsdist.candidate.conf") != 1 {
		t.Errorf("lua candidate not checked:\n%s", h.log())
	}
	if got := h.sup.Args(); strings.Join(got, " ") != "--supervised --disable-syslog -C "+filepath.Join(h.dir, "dnsdist.conf") {
		t.Errorf("args %v", got)
	}
}

func TestSupervisorUser(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.User = "953:953" })
	h.waitFor("ready", h.ready)
	if got := strings.Join(h.sup.Args(), " "); got != "--supervised --disable-syslog --uid 953 --gid 953 -C "+filepath.Join(h.dir, "dnsdist.yml") {
		t.Errorf("args %v", got)
	}
	for _, bad := range []string{":953", "953:", "9 53"} {
		if _, err := New(Config{Source: &cluster.StaticSource{}, User: bad}); err == nil {
			t.Errorf("user %q accepted", bad)
		}
	}
	s, err := New(Config{Source: &cluster.StaticSource{}, User: "pdns"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Args(), " "); !strings.Contains(got, "--uid pdns -C") || strings.Contains(got, "--gid") {
		t.Errorf("uid without gid: args %v", got)
	}
}

func testutilCounter(reg *prometheus.Registry, name, label string) float64 {
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
