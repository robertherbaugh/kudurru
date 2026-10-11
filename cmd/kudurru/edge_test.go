package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/edge/keepalived"
)

func runErr(args ...string) (string, error) {
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestEdgeRender(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "proxy_secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, "edge", "render", "--backends", "node-a=10.0.0.11:5353,node-b=10.0.0.12", "--proxy-protocol-secret-file", secret,
		"--authoritative-zones", "example.com", "--server-id", "node-a")
	for _, want := range []string{"# ---- dnsdist.yml ----", "# ---- dnsdist.lua ----", "address: 10.0.0.12:5353", "proxy_type: 224",
		"value: " + strings.Repeat("ab", 32), "- example.com.", "kudurru_validate_"} {
		if !strings.Contains(out, want) {
			t.Errorf("render output lacks %q", want)
		}
	}
	dir := t.TempDir()
	run(t, "edge", "render", "--backends", "127.0.0.1:5353", "--renderer", "lua", "--out", dir)
	if b, err := os.ReadFile(filepath.Join(dir, "dnsdist.conf")); err != nil || !bytes.Contains(b, []byte(`newServer({address="127.0.0.1:5353"`)) { //nolint:gosec // test-owned path
		t.Errorf("lua render: %v\n%s", err, b)
	}

	for name, args := range map[string][]string{
		"secret required":   {"edge", "render", "--backends", "10.0.0.11"},
		"short secret":      {"edge", "render", "--backends", "10.0.0.11", "--proxy-protocol-secret-file", writeTemp(t, "short")},
		"binary secret":     {"edge", "render", "--backends", "10.0.0.11", "--proxy-protocol-secret-file", writeTemp(t, strings.Repeat("\x00", 40))},
		"no backends":       {"edge", "render"},
		"public api":        {"edge", "render", "--backends", "127.0.0.1", "--api", "0.0.0.0:8083"},
		"unknown protocol":  {"edge", "render", "--backends", "127.0.0.1", "--protocols", "dnscrypt"},
		"tls without certs": {"edge", "render", "--backends", "127.0.0.1", "--protocols", "dot"},
		"empty tls dir":     {"edge", "render", "--backends", "127.0.0.1", "--tls-dir", t.TempDir()},
		"bad source":        {"edge", "render", "--backends-from", "consul"},
		"bad renderer":      {"edge", "render", "--backends", "127.0.0.1", "--renderer", "toml"},
	} {
		if _, err := runErr(args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVIPRender(t *testing.T) {
	out := run(t, "vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--vip", "dns2=10.0.0.54/24@eth0#54",
		"--peers", "10.0.0.11,10.0.0.13", "--priority-index", "1", "--unicast-src", "10.0.0.12", "--router-id", "node-b",
		"--hmac-key-file", "/run/secrets/vrrp_hmac", "--preempt-delay", "dns2=60")
	for _, want := range []string{"router_id node-b", "virtual_router_id 53\n    priority 190", "virtual_router_id 54\n    priority 190",
		"preempt_delay 60", "key 1 file:/run/secrets/vrrp_hmac\n", "/usr/local/bin/kudurru health --vrrp --dns 10.0.0.54:53"} {
		if !strings.Contains(out, want) {
			t.Errorf("vip render lacks %q:\n%s", want, out)
		}
	}
	out = run(t, "vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--peers", "10.0.0.12", "--unicast-src", "10.0.0.11",
		"--router-id", "node-a", "--legacy", "--probe-command", "/usr/local/bin/kudurru vip probe")
	if strings.Contains(out, "auth_hmac {") || !strings.Contains(out, "kudurru vip probe --dns 10.0.0.53:53") {
		t.Errorf("legacy render:\n%s", out)
	}
	for name, args := range map[string][]string{
		"no vip":        {"vip", "render", "--peers", "10.0.0.12", "--unicast-src", "10.0.0.11"},
		"bad vip":       {"vip", "render", "--vip", "dns=10.0.0.53@eth0", "--peers", "10.0.0.12", "--unicast-src", "10.0.0.11"},
		"no peers":      {"vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--unicast-src", "10.0.0.11"},
		"bad preempt":   {"vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--peers", "10.0.0.12", "--unicast-src", "10.0.0.11", "--preempt-delay", "dns3=1"},
		"no such iface": {"vip", "render", "--vip", "dns=10.0.0.53/24@nope0#53", "--peers", "10.0.0.12"},
	} {
		if _, err := runErr(args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVIPDrainAndRebalance(t *testing.T) {
	dir := t.TempDir()
	run(t, "vip", "drain", "--run-dir", dir, "dns")
	if w, _ := keepalived.ReadWeight(dir, "dns"); w != keepalived.WeightFault {
		t.Errorf("drain wrote %d", w)
	}
	run(t, "vip", "drain", "--run-dir", dir, "--undo", "dns")
	if w, _ := keepalived.ReadWeight(dir, "dns"); w != keepalived.WeightServing {
		t.Errorf("undo wrote %d", w)
	}

	if out := run(t, "vip", "rebalance", "--run-dir", dir); !strings.Contains(out, "nothing to rebalance") {
		t.Errorf("empty rebalance: %q", out)
	}
	// node-b holds dns2 (preferred) and dns (priority 190: not preferred).
	cons := &keepalived.Consumer{RunDir: dir, Node: "node-b"}
	for _, l := range []string{`INSTANCE "VI_dns" MASTER 190`, `INSTANCE "VI_dns2" MASTER 200`} {
		if err := cons.Handle(l); err != nil {
			t.Fatal(err)
		}
	}
	// Play keepalived: once the track file says 300, go FAULT.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			if w, _ := keepalived.ReadWeight(dir, "dns"); w == keepalived.WeightFault {
				_ = cons.Handle(`INSTANCE "VI_dns" FAULT 190`)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	out := run(t, "vip", "rebalance", "--run-dir", dir, "--timeout", "5s")
	<-done
	if !strings.Contains(out, "dns: released") || strings.Contains(out, "dns2:") {
		t.Errorf("rebalance output: %q", out)
	}
	if w, _ := keepalived.ReadWeight(dir, "dns"); w != keepalived.WeightServing {
		t.Errorf("weight after rebalance %d, want 0", w)
	}
	// A VIP that never leaves MASTER: error and weight reset.
	if _, err := runErr("vip", "rebalance", "--run-dir", dir, "--vip", "dns2", "--timeout", "300ms"); err == nil {
		t.Error("stuck VIP reported success")
	}
	if w, _ := keepalived.ReadWeight(dir, "dns2"); w != keepalived.WeightServing {
		t.Errorf("stuck VIP weight %d, want reset to 0", w)
	}
}

func TestInstallVRRPHealth(t *testing.T) {
	called := false
	cmd := &cobra.Command{Use: "health", RunE: func(*cobra.Command, []string) error { called = true; return nil }}
	cmd.Flags().Duration("timeout", 3*time.Second, "")
	installVRRPHealth(cmd)
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil || !called {
		t.Fatalf("plain health: %v called=%v", err, called)
	}
	called = false
	cmd.SetArgs([]string{"--vrrp"})
	if err := cmd.Execute(); err == nil || called {
		t.Fatalf("--vrrp without --dns: %v called=%v", err, called)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--vrrp", "--dns", "127.0.0.1:1", "--dnsdist-api", "", "--timeout", "200ms"})
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "target=127.0.0.1:1") {
		t.Fatalf("--vrrp against a closed port: %v %q", err, out.String())
	}
	if _, err := runErr("vip", "probe"); err == nil {
		t.Error("vip probe without --dns accepted")
	}
	// The real health command carries the hook: --vrrp reaches the probe.
	if _, err := runErr("health", "--vrrp"); err == nil || !strings.Contains(err.Error(), "--dns is required") {
		t.Errorf("health --vrrp without --dns: %v", err)
	}
	if o, err := runErr("health", "--vrrp", "--dns", "127.0.0.1:1", "--dnsdist-api", "", "--timeout", "200ms"); err == nil || !strings.Contains(o, "target=127.0.0.1:1") {
		t.Errorf("health --vrrp against a closed port: %v %q", err, o)
	}
}
