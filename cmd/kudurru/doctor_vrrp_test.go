package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorWarnsOnLegacyVRRP(t *testing.T) {
	h := newHarness(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	data := t.TempDir()
	if err := os.Chmod(data, 0o750); err != nil { //nolint:gosec // G302: the mode doctor expects of a dataDir
		t.Fatal(err)
	}
	cfg := doctorConfig(t, data, busy)
	runDir, etc := t.TempDir(), t.TempDir()
	conf := filepath.Join(etc, "keepalived.conf")
	doctor := func() string {
		out, _, _ := h.run("--config", cfg, "doctor", "--skip-api", "--skip-ports", "--vip-run-dir", runDir, "--keepalived-config", conf)
		return out
	}
	// No keepalived on this node: no vrrp line at all.
	if out := doctor(); strings.Contains(out, "vrrp:") {
		t.Errorf("vrrp check without keepalived:\n%s", out)
	}
	vip := []string{"vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--peers", "10.0.0.12", "--unicast-src", "10.0.0.11", "--router-id", "a"}
	authed := run(t, vip...)
	legacy := run(t, append(vip, "--legacy")...)
	if err := os.WriteFile(conf, []byte(authed), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := doctor(); !strings.Contains(out, "vrrp: OK 1 VRRP instances authenticated with auth_hmac") {
		t.Errorf("authenticated keepalived.conf:\n%s", out)
	}
	if err := os.WriteFile(conf, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := doctor(); !strings.Contains(out, "vrrp: WARN "+conf+": 1 of 1 VRRP instances without auth_hmac (VI_dns)") ||
		!strings.Contains(out, "summary: 0 failed, 1 warnings") {
		t.Errorf("legacy keepalived.conf:\n%s", out)
	}
	// The supervisor's marker on the shared run volume is enough.
	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "vrrp-legacy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := doctor(); !strings.Contains(out, "vrrp: WARN `kudurru vip run --legacy` is in use") {
		t.Errorf("legacy marker:\n%s", out)
	}
}
