package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// Rendered credentials are random on every invocation. Compare only after
// removing credential values locally, and never print either rendered body.
var supervisorCredentials = regexp.MustCompile(`(?m)^(?:  (?:key|api_key|password):.*|setKey\(.*|setWebserverConfig\(.*)$`)

func supervisorRender(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runErr(args...)
	if err != nil {
		t.Fatalf("offline supervisor command failed: %v", err)
	}
	return supervisorCredentials.ReplaceAllString(out, "<per-start credentials>")
}

func TestSupervisorEdgeRenderLayers(t *testing.T) {
	for _, renderer := range []string{"yaml", "lua"} {
		t.Run(renderer, func(t *testing.T) {
			base := []string{"edge", "render", "--renderer", renderer, "--server-id", "edge-a", "--backends", "loop=127.0.0.1:5353", "--listen", "127.0.0.1", "--dynblocks", "--dynblocks-qps", "17", "--transfer-sources", "192.0.2.0/24", "--authoritative-zones", "example.org", "--authoritative-cache"}
			want := supervisorRender(t, base...)
			path := writeTemp(t, fmt.Sprintf(`edge:
  renderer: %s
  serverID: edge-a
  backends: [loop=127.0.0.1:5353]
  listen: [127.0.0.1]
  dynblocks: true
  dynblocksQPS: 17
  transferSources: [192.0.2.0/24]
  authoritativeZones: [example.org]
  authoritativeCache: true
`, renderer))
			got := supervisorRender(t, "--config", path, "edge", "render")
			if got != want {
				t.Fatal("file configuration differs from equivalent explicit flags")
			}
			t.Setenv("KUDURRU_EDGE__DYNBLOCKS", "true")
			t.Setenv("KUDURRU_EDGE__DYNBLOCKSQPS", "17")
			t.Setenv("KUDURRU_EDGE__AUTHORITATIVECACHE", "true")
			t.Setenv("KUDURRU_EDGE__TRANSFERSOURCES", "192.0.2.0/24")
			t.Setenv("KUDURRU_EDGE__AUTHORITATIVEZONES", "example.org")
			t.Setenv("KUDURRU_EDGE__LISTEN", "127.0.0.1")
			t.Setenv("KUDURRU_EDGE__BACKENDS", "loop=127.0.0.1:5353")
			got = supervisorRender(t, "edge", "render", "--renderer", renderer, "--server-id", "edge-a")
			if got != want {
				t.Fatal("environment configuration differs from equivalent explicit flags")
			}
		})
	}
}

func TestSupervisorVIPRenderLayers(t *testing.T) {
	flags := []string{"vip", "render", "--vip", "dns=192.0.2.53/24@eth0#53", "--vip", "dns2=192.0.2.54/24@eth1#54", "--preempt-delay", "dns2=30", "--unicast-src", "192.0.2.11", "--peers", "192.0.2.12,192.0.2.13", "--priority-index", "1", "--node-name", "node-b", "--vrrp-interface", "eth2"}
	want := supervisorRender(t, flags...)
	path := writeTemp(t, `node: {name: node-b}
vip:
  vips:
    - {name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 53}
    - {name: dns2, address: 192.0.2.54/24, interface: eth1, vrid: 54}
  preemptDelay: {dns2: 30}
  unicastSrc: 192.0.2.11
  peers: [192.0.2.12, 192.0.2.13]
  priorityIndex: 1
  vrrpInterface: eth2
`)
	if got := supervisorRender(t, "--config", path, "vip", "render"); got != want {
		t.Fatal("typed VIP file differs from existing flag rendering")
	}
	t.Setenv("KUDURRU_VIP__VIPS", `[{"name":"dns","address":"192.0.2.53/24","interface":"eth0","vrid":53},{"name":"dns2","address":"192.0.2.54/24","interface":"eth1","vrid":54}]`)
	t.Setenv("KUDURRU_VIP__PREEMPTDELAY", `{"dns2":30}`)
	t.Setenv("KUDURRU_VIP__UNICASTSRC", "192.0.2.11")
	t.Setenv("KUDURRU_VIP__PEERS", "192.0.2.12,192.0.2.13")
	t.Setenv("KUDURRU_VIP__PRIORITYINDEX", "1")
	t.Setenv("KUDURRU_VIP__VRRPINTERFACE", "eth2")
	t.Setenv("KUDURRU_VIP__ROUTERID", "node-b")
	if got := supervisorRender(t, "vip", "render"); got != want {
		t.Fatal("typed VIP environment differs from existing flag rendering")
	}
	// Explicit --vip replaces a stale invalid lower array before decode, and
	// --preempt-delay replaces the whole lower map, including removed VIPs.
	replacement := writeTemp(t, "vip: {vips: invalid, preemptDelay: {removed: 99}}\n")
	t.Setenv("KUDURRU_VIP__VIPS", "invalid")
	t.Setenv("KUDURRU_VIP__PREEMPTDELAY", "invalid")
	if got := supervisorRender(t, append([]string{"--config", replacement}, flags...)...); got != want {
		t.Fatal("explicit VIP replacement did not repair lower layers")
	}
}

func TestSupervisorExplicitFlagsOnlySkipTheirEnvironment(t *testing.T) {
	path := writeTemp(t, "edge: {backends: [127.0.0.9], dynblocks: true, authoritativeZones: [stale.example], renderer: invalid}\n")
	t.Setenv("KUDURRU_EDGE__THREADS", "invalid")
	t.Setenv("KUDURRU_EDGE__BACKENDS_FILE", filepath.Join(t.TempDir(), "missing"))
	out := supervisorRender(t, "--config", path, "edge", "render", "--threads=2", "--backends=127.0.0.1:5353", "--dynblocks=false", "--authoritative-zones=", "--renderer=yaml")
	if !strings.Contains(out, "address: 127.0.0.1:5353") || strings.Contains(out, "127.0.0.9") || strings.Contains(out, "stale.example") || strings.Contains(out, "dyn_blocks:") {
		t.Fatal("explicit bool/list replacement did not win")
	}
	for _, args := range [][]string{
		{"edge", "render", "--backends=127.0.0.1"}, // threads not explicitly changed
		{"edge", "render", "--threads=2"},          // missing _FILE not explicitly overridden
	} {
		if _, err := runErr(args...); err == nil {
			t.Fatal("unrelated/default flag suppressed invalid lower environment")
		}
	}
	t.Setenv("KUDURRU_EDGE__UNKNOWN", "bad")
	if _, err := runErr("edge", "render", "--threads=2", "--backends=127.0.0.1"); err == nil {
		t.Fatal("unknown environment key bypassed")
	}
}

func TestSupervisorRunHydrationAndFlagContract(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"edge", "vip"} {
		t.Run(group, func(t *testing.T) {
			cmd := &cobra.Command{}
			mapping := edgeSettings
			if group == "edge" {
				var f edgeFlags
				f.register(cmd, true)
			} else {
				var f vipFlags
				f.register(cmd, true)
				mapping = vipSettings
			}
			// Every registered run flag maps to one schema key; every supervisor
			// schema key is reachable from a flag. Defaults stay backward compatible.
			mapped := map[string]bool{}
			cmd.Flags().VisitAll(func(flag *pflag.Flag) {
				path, ok := mapping[flag.Name]
				if !ok {
					t.Errorf("unmapped flag --%s", flag.Name)
					return
				}
				key, ok := config.Lookup(path)
				if !ok {
					t.Errorf("unknown mapped key %s", path)
					return
				}
				mapped[path] = true
				if flag.Name == "vip" || flag.Name == "preempt-delay" || flag.Name == "backends" || flag.Value.Type() == "stringSlice" {
					return
				}
				if fmt.Sprint(key.Default) != flag.DefValue {
					t.Errorf("default drift for %s", path)
				}
			})
			for _, key := range config.Schema {
				if strings.HasPrefix(key.Path, group+".") && !strings.HasPrefix(key.Path, "vip.observation.") && !mapped[key.Path] {
					t.Errorf("unreachable setting %s", key.Path)
				}
			}
		})
	}
	if cfg.Edge.PollZones != true {
		t.Fatal("polling default changed")
	}
	// Exercise the actual run PreRunE wiring without starting an executable.
	path := writeTemp(t, `edge: {backends: [127.0.0.1], configDir: /tmp/edge-local, healthAddress: ':8184', pollZones: true, drain: 3s, internalPort: 8082}
vip: {routerID: file-node, healthAddress: ':8185', keepalivedBin: /host/keepalived, stopTimeout: 9s}
`)
	t.Setenv("KUDURRU_EDGE__DRAIN", "4s")
	t.Setenv("KUDURRU_EDGE__HEALTHADDRESS_FILE", filepath.Join(t.TempDir(), "missing-health"))
	root := newRoot()
	cmd, rest, err := root.Find([]string{"--config", path, "edge", "run", "--readiness-address=:8284", "--poll-zones=false", "--internal-port=0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	if err := cmd.PreRunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{"config-dir": "/tmp/edge-local", "health-address": ":8284", "readiness-address": ":8284", "poll-zones": "false", "internal-port": "0", "drain": "4s"} {
		if got := cmd.Flags().Lookup(flag).Value.String(); got != want {
			t.Errorf("run hydration %s=%s", flag, got)
		}
	}
	for _, flag := range []string{"config-dir", "health-address", "drain"} {
		if cmd.Flags().Changed(flag) {
			t.Errorf("hydration fabricated explicit flag %s", flag)
		}
	}
	t.Setenv("KUDURRU_EDGE__HEALTHADDRESS_FILE", writeTemp(t, ":8184"))
	t.Setenv("KUDURRU_VIP__ROUTERID_FILE", filepath.Join(t.TempDir(), "missing-router"))
	root = newRoot()
	cmd, rest, err = root.Find([]string{"--config", path, "vip", "run", "--node-name=chart-node", "--stop-timeout=0s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	if err := cmd.PreRunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{"router-id": "chart-node", "node-name": "chart-node", "health-address": ":8185", "keepalived-bin": "/host/keepalived", "stop-timeout": "0s"} {
		if cmd.Flags().Lookup(flag).Value.String() != want {
			t.Errorf("VIP run hydration failed for %s", flag)
		}
	}
}

func TestSupervisorConfiguredPrioritySurvivesDiscovery(t *testing.T) {
	node := func(name, addr string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"vip": "true"}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: addr}}}}
	}
	cs := fake.NewClientset(node("node-a", "192.0.2.11"), node("node-b", "192.0.2.12"))
	old := nodesClient
	nodesClient = func() (corev1client.NodesGetter, error) { return cs.CoreV1(), nil }
	t.Cleanup(func() { nodesClient = old })
	common := `node: {name: node-b}
vip:
  vips: [{name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 53}]
  peersFrom: 'nodes:vip=true'
  unicastSrc: 192.0.2.12
`
	for _, layer := range []string{"omitted", "file", "environment", "flag"} {
		t.Run(layer, func(t *testing.T) {
			body := common
			if layer == "file" {
				body += "  priorityIndex: 0\n"
			}
			args := []string{"--config", writeTemp(t, body), "vip", "render"}
			if layer == "environment" {
				t.Setenv("KUDURRU_VIP__PRIORITYINDEX", "0")
			}
			if layer == "flag" {
				args = append(args, "--priority-index=0")
			}
			want := "priority 200"
			if layer == "omitted" {
				want = "priority 190"
			}
			if !strings.Contains(supervisorRender(t, args...), want) {
				t.Fatal("explicit priority lost to discovered index")
			}
		})
	}
}

func TestSupervisorConfigRefusals(t *testing.T) {
	//nolint:gosec // G101: refusal fixtures name missing credential keys, not secrets.
	for name, body := range map[string]string{
		"public API":           "edge: {backends: [127.0.0.1], api: '0.0.0.0:8083'}",
		"public console":       "edge: {backends: [127.0.0.1], console: '0.0.0.0:5199'}",
		"missing proxy secret": "edge: {backends: [192.0.2.10]}",
		"missing TLS":          "edge: {backends: [127.0.0.1], protocols: [dot]}",
		"unknown source":       "edge: {backendsFrom: consul}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runErr("--config", writeTemp(t, body), "edge", "render"); err == nil {
				t.Fatal("configured edge bypassed existing renderer/source/credential refusal")
			}
		})
	}
	// Explicitly requested missing files must still fail; implicit default
	// absence continues to support existing Compose and chart invocations.
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := runErr("--config", missing, "edge", "render", "--backends=127.0.0.1"); err == nil {
		t.Fatal("explicit missing config accepted")
	}
	t.Setenv("KUDURRU_CONFIG", missing)
	if _, err := runErr("vip", "render"); err == nil {
		t.Fatal("explicit environment config absence accepted")
	}
}

// Verify every supported layout's existing run flags still resolve through the
// new loader. This remains offline: no license bypass or second runtime exists.
func TestSupervisorDeploymentDefaults(t *testing.T) {
	for _, args := range [][]string{
		{"edge", "run", "--backends=127.0.0.1:5353", "--pool-resolver=chashed", "--pool-authoritative=leastOutstanding", "--tls-dir=/certs", "--api=127.0.0.1:8083", "--console=127.0.0.1:5199", "--api-key-out=/run/kudurru/dnsdist-api.key"},
		{"edge", "run", "--backends-from=endpointslice:kudurru-system/kudurru-headless", "--backend-port=5353", "--readiness-address=0.0.0.0:8084", "--tls-dir=/certs", "--dot=0.0.0.0:853", "--doh=0.0.0.0:443", "--doq=0.0.0.0:853", "--doh3=0.0.0.0:443"},
		{"edge", "run", "--backends=node=127.0.0.1:5353"},
		{"vip", "run", "--vip=dns=192.0.2.53/24@eth0#53", "--node-name=worker-1", "--peers-from=nodes:kudurru.io/vip=true", "--hmac-key-file=/etc/kudurru/secrets/vrrp/vrrp-hmac", "--track-dnsdist-api=http://127.0.0.1:8083"},
	} {
		root := newRoot()
		cmd, rest, err := root.Find(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Fatal(err)
		}
		if err := cmd.PreRunE(cmd, nil); err != nil {
			t.Fatal(err)
		}
	}
	// No command gained an --out setting or a server-starting enabled key.
	for _, path := range []string{"edge.out", "edge.enabled", "vip.enabled"} {
		if _, ok := config.Lookup(path); ok {
			t.Errorf("unexpected runtime/output setting %s", path)
		}
	}
}

func TestSupervisorRendererCompatibility(t *testing.T) {
	for _, renderer := range []string{"", "YAML", "yml", "LUA"} {
		out := supervisorRender(t, "edge", "render", "--backends=127.0.0.1", "--renderer="+renderer, "--protocols=Do53")
		if !strings.Contains(out, "dnsdist.") {
			t.Fatal("existing renderer alias or protocol casing refused")
		}
	}
}

func TestSupervisorConfiguredTLSFrontendParity(t *testing.T) {
	dir := t.TempDir()
	writeTestCert(t, dir)
	flags := []string{"edge", "render", "--backends=127.0.0.1:5353", "--tls-dir=" + dir, "--dot=0.0.0.0:8853", "--doh=0.0.0.0:8443", "--doq=0.0.0.0:8854", "--doh3=0.0.0.0:8444"}
	want := supervisorRender(t, flags...)
	path := writeTemp(t, fmt.Sprintf("edge: {backends: [127.0.0.1:5353], tlsDir: %q, dot: '0.0.0.0:8853', doh: '0.0.0.0:8443', doq: '0.0.0.0:8854', doh3: '0.0.0.0:8444'}", dir))
	if got := supervisorRender(t, "--config", path, "edge", "render"); got != want {
		t.Fatal("configured chart-style TLS addresses changed rendering")
	}
	bad := writeTemp(t, fmt.Sprintf("edge: {backends: [127.0.0.1], tlsDir: %q, dot: '192.0.2.1:853'}", dir))
	if _, err := runErr("--config", bad, "edge", "render"); err == nil {
		t.Fatal("configured frontend outside listen addresses accepted")
	}
}

func TestSupervisorExplicitClearsKeepRefusalAndAuthentication(t *testing.T) {
	t.Setenv("KUDURRU_EDGE__PROXYPROTOCOLSECRETFILE_FILE", filepath.Join(t.TempDir(), "missing-secret-reference"))
	if _, err := runErr("edge", "render", "--backends=192.0.2.10", "--proxy-protocol-secret-file="); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatal("clearing an overridden secret reference bypassed the nonloopback credential gate")
	}
	// Supply a valid lower reference for later commands: unrelated invalid
	// environment is deliberately not suppressed when loading VIP settings.
	t.Setenv("KUDURRU_EDGE__PROXYPROTOCOLSECRETFILE_FILE", writeTemp(t, ""))
	path := writeTemp(t, `vip:
  vips: [{name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 53}]
  peers: [192.0.2.12]
  unicastSrc: 192.0.2.11
  legacy: true
  preemptDelay: {dns: 30}
`)
	out := supervisorRender(t, "--config", path, "vip", "render", "--legacy=false", "--preempt-delay=")
	if !strings.Contains(out, "auth_hmac {") || !strings.Contains(out, "nopreempt") || strings.Contains(out, "preempt_delay 30") {
		t.Fatal("explicit false/empty override weakened authentication or failed to replace delays")
	}
	if _, err := runErr("--config", path, "vip", "render", "--vip=", "--preempt-delay="); err == nil {
		t.Fatal("empty explicit VIP list silently reused configured addresses")
	}
}
