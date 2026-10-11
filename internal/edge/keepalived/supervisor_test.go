package keepalived

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func link(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.Symlink(self, p); err != nil {
		t.Fatal(err)
	}
	return p
}

type khar struct {
	t      *testing.T
	sup    *Supervisor
	etc    string
	run    string
	cancel context.CancelFunc
	done   chan error
	mu     sync.Mutex
	events []Event
}

func newKHarness(t *testing.T, mutate func(*SupervisorConfig)) *khar {
	t.Helper()
	root := t.TempDir()
	h := &khar{t: t, etc: filepath.Join(root, "etc"), run: filepath.Join(root, "run")}
	c := goldenConfig(t, 0, false)
	c.RunDir = h.run
	c.Binary = link(t, "kudurru")
	reg := prometheus.NewRegistry()
	cfg := SupervisorConfig{
		Config: c, ConfigPath: filepath.Join(h.etc, "keepalived.conf"), Binary: link(t, "keepalived"),
		StatePoll: 20 * time.Millisecond, StopTimeout: 5 * time.Second,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registerer: reg, Gatherer: reg,
		OnEvent: func(e Event) { h.mu.Lock(); h.events = append(h.events, e); h.mu.Unlock() },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.sup = sup
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- sup.Run(ctx) }()
	t.Cleanup(h.stop)
	return h
}

func (h *khar) stop() {
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

func (h *khar) log() string {
	b, _ := os.ReadFile(filepath.Join(h.etc, "fake.log"))
	return string(b)
}

func (h *khar) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; fake log:\n%s", what, h.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *khar) metrics() string {
	rec := httptest.NewRecorder()
	h.sup.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestSupervisorFIFOAndReload(t *testing.T) {
	h := newKHarness(t, nil)
	h.waitFor("start", func() bool { return strings.Contains(h.log(), "start pid=") })
	if !strings.HasPrefix(h.log(), "check keepalived.candidate.conf\n") {
		t.Errorf("the candidate was not checked before the start:\n%s", h.log())
	}
	// FIFO → `vip fifo` → state file → metrics and events.
	h.waitFor("MASTER on dns", func() bool {
		return strings.Contains(h.metrics(), `kudurru_vip_master{vip="dns"} 1`) && strings.Contains(h.metrics(), `kudurru_vip_master{vip="dns2"} 0`)
	})
	st, err := ReadState(h.run)
	if err != nil || st.Node != "node-a" || st.VIPs["dns"].State != StateMaster || st.VIPs["dns"].Priority != 200 || st.VIPs["dns2"].State != StateBackup {
		t.Fatalf("state %+v %v", st, err)
	}
	h.mu.Lock()
	var sawMaster bool
	for _, e := range h.events {
		if e.VIP == "dns" && e.State == StateMaster && e.Node == "node-a" {
			sawMaster = true
		}
	}
	h.mu.Unlock()
	if !sawMaster {
		t.Errorf("no MASTER event: %+v", h.events)
	}

	// Re-render with a new peer → keepalived -t → swap → SIGHUP.
	c := goldenConfig(t, 0, false)
	c.RunDir, c.Binary = h.run, h.sup.cfg.Config.Binary
	c.Peers = []string{"10.0.0.12", "10.0.0.13", "10.0.0.14"}
	if err := h.sup.Rerender(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h.waitFor("hup", func() bool { return strings.Contains(h.log(), "\nhup\n") })
	conf, _ := os.ReadFile(filepath.Join(h.etc, "keepalived.conf"))
	if !strings.Contains(string(conf), "10.0.0.14") {
		t.Error("new peer not rendered")
	}
	if _, err := os.Stat(filepath.Join(h.etc, "keepalived.conf.prev")); err != nil {
		t.Errorf("previous config not kept: %v", err)
	}
	// Unchanged re-render: no signal.
	if err := h.sup.Rerender(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// A VRID change is refused, never applied.
	bad := c
	bad.VIPs = append([]VIP(nil), c.VIPs...)
	bad.VIPs[0].VRID = 99
	if err := h.sup.Rerender(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "host-only") {
		t.Errorf("VRID change: %v", err)
	}
	// A configuration keepalived -t rejects is never installed.
	rejected := c
	rejected.HMACKeyFile = "/run/secrets/FAIL-CHECK"
	if err := h.sup.Rerender(context.Background(), rejected); err == nil {
		t.Error("rejected configuration installed")
	}
	conf, _ = os.ReadFile(filepath.Join(h.etc, "keepalived.conf"))
	if strings.Contains(string(conf), "FAIL-CHECK") || strings.Contains(string(conf), "virtual_router_id 99") {
		t.Error("an invalid configuration reached keepalived.conf")
	}
	if n := strings.Count(h.log(), "\nhup"); n != 1 {
		t.Errorf("%d SIGHUPs, want 1:\n%s", n, h.log())
	}

	h.stop()
	if !strings.Contains(h.log(), "stop pid=") {
		t.Errorf("keepalived did not get SIGTERM:\n%s", h.log())
	}
	st, _ = ReadState(h.run)
	if st.VIPs["dns"].State != StateStop {
		t.Errorf("state after stop: %+v", st.VIPs)
	}
}

func TestSupervisorPeersFile(t *testing.T) {
	peers := filepath.Join(t.TempDir(), "peers")
	if err := os.WriteFile(peers, []byte("# nodes\n10.0.0.12\n10.0.0.13\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newKHarness(t, func(c *SupervisorConfig) { c.PeersFile = peers })
	h.waitFor("start", func() bool { return strings.Contains(h.log(), "start pid=") })
	if err := os.WriteFile(peers, []byte("10.0.0.12, 10.0.0.13 10.0.0.15\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.waitFor("hup after the peers file changed", func() bool { return strings.Contains(h.log(), "\nhup\n") })
	conf, _ := os.ReadFile(filepath.Join(h.etc, "keepalived.conf"))
	if !strings.Contains(string(conf), "10.0.0.15") {
		t.Error("peer from the file not rendered")
	}
}

func TestSupervisorInitialCheckFailure(t *testing.T) {
	c := goldenConfig(t, 0, false)
	c.RunDir = t.TempDir()
	c.HMACKeyFile = "/FAIL-CHECK"
	reg := prometheus.NewRegistry()
	sup, err := NewSupervisor(SupervisorConfig{Config: c, ConfigPath: filepath.Join(t.TempDir(), "keepalived.conf"), Binary: link(t, "keepalived"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Registerer: reg, Gatherer: reg})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Run(context.Background()); err == nil {
		t.Fatal("Run started keepalived with a configuration keepalived -t rejected")
	}
}

func TestParseLine(t *testing.T) {
	tests := map[string]Notification{
		`INSTANCE "VI_dns" MASTER 200`:           {Kind: "INSTANCE", Name: "VI_dns", State: "MASTER", Priority: 200},
		`GROUP "G1" BACKUP 0`:                    {Kind: "GROUP", Name: "G1", State: "BACKUP"},
		`INSTANCE "VI_x" MASTER_RX_LOWER_PRI 90`: {Kind: "INSTANCE", Name: "VI_x", State: "MASTER_RX_LOWER_PRI", Priority: 90},
		"INSTANCE \"VI_dns\" FAULT 0\n":          {Kind: "INSTANCE", Name: "VI_dns", State: "FAULT"},
	}
	for in, want := range tests {
		got, err := ParseLine(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "HELLO", `INSTANCE VI_dns MASTER 1`, `INSTANCE "VI_dns`, `INSTANCE "VI_dns"`, `INSTANCE "VI_dns" MASTER x`} {
		if _, err := ParseLine(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConsumer(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var changes []string
	c := &Consumer{RunDir: dir, Node: "node-a", Now: func() time.Time { now = now.Add(time.Second); return now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnChange: func(vip string, prev, next VIPState) {
			changes = append(changes, vip+":"+prev.State+">"+next.State)
		}}
	input := strings.Join([]string{
		`INSTANCE "VI_dns" BACKUP 200`,
		`INSTANCE "VI_dns" MASTER 200`,
		`INSTANCE "VI_dns" MASTER 200`, // duplicate: no change
		`INSTANCE "VI_dns" MASTER_RX_LOWER_PRI 190`,
		`INSTANCE "VI_dns" MASTER_PRIORITY 140`,
		`GROUP "G" FAULT 0`,
		`INSTANCE "other" MASTER 1`,
		`not a line`,
		`INSTANCE "VI_dns" FAULT 0`,
	}, "\n")
	if err := c.Run(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	want := []string{"dns:>BACKUP", "dns:BACKUP>MASTER", "dns:MASTER>MASTER", "dns:MASTER>FAULT"}
	if strings.Join(changes, ",") != strings.Join(want, ",") {
		t.Errorf("changes %v, want %v", changes, want)
	}
	st, err := ReadState(dir)
	if err != nil || st.VIPs["dns"].State != StateFault || st.Node != "node-a" {
		t.Errorf("state %+v %v", st, err)
	}
	// MASTER since is kept across the priority change; FAULT resets it.
	if !st.VIPs["dns"].Since.Equal(st.Updated) {
		t.Errorf("since %v updated %v", st.VIPs["dns"].Since, st.Updated)
	}
}

func TestTrackFile(t *testing.T) {
	dir := t.TempDir()
	if w, err := ReadWeight(dir, "dns"); err != nil || w != WeightServing {
		t.Fatalf("missing file: %d %v", w, err)
	}
	for _, w := range []int{WeightFault, WeightPreferNot, WeightServing} {
		if err := WriteWeight(dir, "dns", w); err != nil {
			t.Fatal(err)
		}
		got, err := ReadWeight(dir, "dns")
		if err != nil || got != w {
			t.Errorf("wrote %d, read %d %v", w, got, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "weight-dns"))
	if string(b) != "0\n" {
		t.Errorf("file content %q", b)
	}
	if err := WriteWeight(dir, "dns", -5); err == nil {
		t.Error("negative weight accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "weight-x"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWeight(dir, "x"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestReadPeersFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "peers")
	_ = os.WriteFile(p, []byte("10.0.0.13 # c\n\n10.0.0.12,10.0.0.13\n"), 0o600)
	got, err := ReadPeersFile(p)
	if err != nil || strings.Join(got, ",") != "10.0.0.12,10.0.0.13" {
		t.Errorf("%v %v", got, err)
	}
	_ = os.WriteFile(p, []byte("node-b\n"), 0o600)
	if _, err := ReadPeersFile(p); err == nil {
		t.Error("host name accepted")
	}
}
