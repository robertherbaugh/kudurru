package keepalived

import (
	"bytes"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func mustVIP(t *testing.T, s string) VIP {
	t.Helper()
	v, err := ParseVIP(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func goldenConfig(t *testing.T, idx int, legacy bool) Config {
	return Config{
		RouterID:      []string{"node-a", "node-b", "node-c"}[idx],
		VIPs:          []VIP{mustVIP(t, "dns=10.0.0.53/24@eth0#53"), mustVIP(t, "dns2=10.0.0.54/24@eth0#54")},
		PriorityIndex: idx,
		UnicastSrc:    []string{"10.0.0.11", "10.0.0.12", "10.0.0.13"}[idx],
		Peers:         map[int][]string{0: {"10.0.0.12", "10.0.0.13"}, 1: {"10.0.0.11", "10.0.0.13"}, 2: {"10.0.0.11", "10.0.0.12"}}[idx],
		HMACKeyFile:   "/run/secrets/vrrp_hmac_{vrid}",
		Legacy:        legacy,
	}
}

func TestRenderGolden(t *testing.T) {
	cases := map[string]Config{
		"two-vips-node-a.conf": goldenConfig(t, 0, false),
		"two-vips-node-c.conf": goldenConfig(t, 2, false),
		"legacy.conf":          goldenConfig(t, 1, true),
	}
	preempt := goldenConfig(t, 0, false)
	preempt.VIPs = []VIP{mustVIP(t, "dns=10.0.0.53/24@eth0#53")}
	preempt.VIPs[0].PreemptDelay = 60
	preempt.RunDir = "/var/run/kudurru"
	preempt.Probe.Command = []string{"/usr/local/bin/kudurru", "health", "--vrrp"}
	cases["preempt-delay.conf"] = preempt
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Render(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name)
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // golden file
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file:\n%s", path, got)
			}
			if err := checkSyntax(got); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRenderInvariants(t *testing.T) {
	a, _ := Render(goldenConfig(t, 0, false))
	c, _ := Render(goldenConfig(t, 2, false))
	l, _ := Render(goldenConfig(t, 1, true))
	for _, want := range []string{
		"enable_script_security", "script_user keepalived_script", "vrrp_version 3", "vrrp_garp_master_refresh 30",
		"vrrp_notify_fifo /run/kudurru/vrrp.fifo", `vrrp_notify_fifo_script "/usr/local/bin/kudurru vip fifo --node node-a"`,
		"state BACKUP", "nopreempt", "advert_int 1", "unicast_fault_no_peer", "check_unicast_src",
		"key 1 file:/run/secrets/vrrp_hmac_53", "key 1 file:/run/secrets/vrrp_hmac_54", "anti_replay monotonic",
		"10.0.0.53/24 dev eth0", "interval 1", "timeout 1", "fall 3", "rise 2", "weight 0",
		`file "/run/kudurru/weight-dns"`, "weight -1",
		"--dns 10.0.0.53:53 --qname health.kudurru.internal. --dnsdist-api http://127.0.0.1:8083 --dnsdist-api-key-file /run/kudurru/dnsdist-api.key --require-backends 1",
	} {
		if !bytes.Contains(a, []byte(want)) {
			t.Errorf("node-a config lacks %q", want)
		}
	}
	// Reversed priorities: node-a prefers dns, node-c prefers dns2.
	if !bytes.Contains(a, []byte("virtual_router_id 53\n    priority 200")) || !bytes.Contains(a, []byte("virtual_router_id 54\n    priority 180")) {
		t.Errorf("node-a priorities wrong:\n%s", a)
	}
	if !bytes.Contains(c, []byte("virtual_router_id 53\n    priority 180")) || !bytes.Contains(c, []byte("virtual_router_id 54\n    priority 200")) {
		t.Errorf("node-c priorities wrong:\n%s", c)
	}
	if bytes.Contains(l, []byte("auth_hmac {")) || !bytes.Contains(l, []byte("check_unicast_src")) || !bytes.Contains(l, []byte("unicast_fault_no_peer")) {
		t.Errorf("legacy mode must drop auth_hmac and keep the unicast checks:\n%s", l)
	}
	if bytes.Contains(a, []byte("priority 255")) {
		t.Error("priority 255 rendered")
	}
}

func TestRenderSeparateVRRPInterface(t *testing.T) {
	c := goldenConfig(t, 0, false)
	c.VRRPInterface = "ens18"
	c.VIPs = []VIP{mustVIP(t, "dns=10.200.200.25/32@ens19#53"), mustVIP(t, "dns2=10.200.200.26/32@ens19#54")}
	b, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Count(text, "    interface ens18\n") != 2 || strings.Contains(text, "    interface ens19\n") {
		t.Fatalf("VRRP must use the control interface for both instances:\n%s", text)
	}
	for _, want := range []string{"10.200.200.25/32 dev ens19", "10.200.200.26/32 dev ens19", "unicast_src_ip 10.0.0.11"} {
		if !strings.Contains(text, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Count(text, "    track_interface {\n        ens19\n    }") != 2 {
		t.Fatal("both VIP instances must track the DMZ link")
	}
}

func TestPriority(t *testing.T) {
	c := Config{NodeCount: 3}
	for idx, want := range [][2]int{{200, 180}, {190, 190}, {180, 200}} {
		c.PriorityIndex = idx
		if got := [2]int{c.Priority(0), c.Priority(1)}; got != want {
			t.Errorf("index %d: %v, want %v", idx, got, want)
		}
	}
}

func TestParseVIP(t *testing.T) {
	v, err := ParseVIP("dns=10.0.0.53/24@eth0#53")
	if err != nil || v.Name != "dns" || v.Prefix != netip.MustParsePrefix("10.0.0.53/24") || v.Interface != "eth0" || v.VRID != 53 {
		t.Fatalf("%+v %v", v, err)
	}
	if v, err := ParseVIP("v6=2001:db8::53/64@bond0.10#7"); err != nil || v.Interface != "bond0.10" {
		t.Fatalf("v6: %+v %v", v, err)
	}
	for _, bad := range []string{"", "dns=10.0.0.53/24@eth0", "dns=10.0.0.53@eth0#53", "dns=10.0.0.53/24@eth0#0",
		"dns=10.0.0.53/24@eth0#256", "DNS=10.0.0.53/24@eth0#53", "dns=127.0.0.1/8@lo#1", `dns"=1.2.3.4/24@eth0#1`} {
		if _, err := ParseVIP(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	base := goldenConfig(t, 0, false)
	mut := map[string]func(*Config){
		"no peers":           func(c *Config) { c.Peers = nil },
		"self peer":          func(c *Config) { c.Peers = []string{"10.0.0.11"} },
		"family mismatch":    func(c *Config) { c.Peers = []string{"2001:db8::1"} },
		"duplicate vrid":     func(c *Config) { c.VIPs[1].VRID = 53 },
		"duplicate name":     func(c *Config) { c.VIPs[1].Name = "dns" },
		"duplicate address":  func(c *Config) { c.VIPs[1].Prefix = netip.MustParsePrefix("10.0.0.53/32") },
		"bad index":          func(c *Config) { c.PriorityIndex = 3 },
		"bad router id":      func(c *Config) { c.RouterID = "node a" },
		"bad vrrp interface": func(c *Config) { c.VRRPInterface = "ens18\n    priority 255" },
		"quote in path":      func(c *Config) { c.HMACKeyFile = `/x"y` },
		"no vips":            func(c *Config) { c.VIPs = nil },
		"bad unicast src":    func(c *Config) { c.UnicastSrc = "eth0" },
		"injected command":   func(c *Config) { c.Probe.Command = []string{"/bin/sh", "-c", "rm -rf /"} },
	}
	for name, m := range mut {
		c := base
		c.VIPs = append([]VIP(nil), base.VIPs...)
		m(&c)
		if _, err := Render(c); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

// TestValidateUnicastAddresses: addresses a VRRP advert cannot be sent
// from or to are refused as the unicast source or as a peer.
func TestValidateUnicastAddresses(t *testing.T) {
	base := goldenConfig(t, 0, false)
	for _, bad := range []string{"0.0.0.0", "0.0.0.1", "255.255.255.255", "224.0.0.18", "127.0.0.2", "::", "::1", "ff02::12", "::ffff:10.0.0.12", "fe80::2%eth0", "fe80::2%eth0}"} {
		c := base
		c.Peers = []string{"10.0.0.12", bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("peer %q", bad)) {
			t.Errorf("peer %s: %v", bad, err)
		}
		c = base
		c.UnicastSrc = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unicast source %q", bad)) {
			t.Errorf("unicast source %s: %v", bad, err)
		}
	}
	c := base
	c.Peers = []string{"10.0.0.12", "192.0.2.1"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid peers refused: %v", err)
	}
}

// checkSyntax is a structural check of keepalived.conf: balanced braces,
// balanced quotes per line, every block keyword known. The real parser
// runs in `make edge-check` (keepalived -t inside the pinned image).
func checkSyntax(b []byte) error {
	depth := 0
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Count(line, `"`)%2 != 0 {
			return errorf("line %d: unbalanced quotes: %q", i+1, line)
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth < 0 {
			return errorf("line %d: unbalanced braces", i+1)
		}
	}
	if depth != 0 {
		return errorf("unbalanced braces at EOF (depth %d)", depth)
	}
	return nil
}
