package deploy_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
)

func TestChartMultipleVIPsAndSupervisorDiscovery(t *testing.T) {
	values := `edge: {enabled: true}
vip:
  provider: keepalived
  keepalived:
    interface: eth1
    vrrpInterface: eth0
    vips:
      - {name: dns-a, address: 10.35.10.53/24, vrid: 53}
      - {name: dns-b, address: 10.35.10.54/24, interface: eth2, vrid: 54, preemptDelay: 30}
`
	out, err := renderChart(t, values)
	if err != nil {
		t.Fatal(err)
	}
	r := newRendered(t, out)
	r.checkGeneric(t)
	pod := mapOf(dig(r.wantEdge(t, "DaemonSet"), "spec", "template"))
	args := list(r.container(t, pod, "keepalived")["args"])
	for _, want := range []string{"--vip=dns-a=10.35.10.53/24@eth1#53", "--vip=dns-b=10.35.10.54/24@eth2#54", "--vrrp-interface=eth0", "--preempt-delay=dns-b=30"} {
		if !slices.Contains(args, any(want)) {
			t.Errorf("args lack %s: %v", want, args)
		}
	}
	svc := r.object(t, "Service", "kudurru-edge-status")
	if dig(svc, "spec", "clusterIP") != "None" || dig(svc, "spec", "publishNotReadyAddresses") != true || len(list(dig(svc, "spec", "ports"))) != 2 {
		t.Fatalf("discovery service = %v", svc)
	}
	server := r.container(t, r.pod(t), "kudurru")
	found := false
	for _, e := range list(server["env"]) {
		if dig(e, "name") == "KUDURRU_EDGE_SUPERVISORS" {
			found = dig(e, "value") == "endpointslice:kudurru-system/kudurru-edge-status"
		}
	}
	if !found {
		t.Fatal("server lacks supervisor discovery source")
	}
	for name, bad := range map[string]string{
		"duplicate name":                          strings.Replace(values, "name: dns-b", "name: dns-a", 1),
		"duplicate VRID":                          strings.Replace(values, "vrid: 54", "vrid: 53", 1),
		"duplicate address":                       strings.Replace(values, "10.35.10.54/24", "10.35.10.53/24", 1),
		"duplicate address with different prefix": strings.Replace(values, "10.35.10.54/24", "10.35.10.53/32", 1),
		"VRID out of range":                       strings.Replace(values, "vrid: 54", "vrid: 256", 1),
		"missing VRID":                            strings.Replace(values, ", vrid: 54", "", 1),
		"invalid delay":                           strings.Replace(values, "preemptDelay: 30", "preemptDelay: -1", 1),
		"invalid interface":                       strings.Replace(values, "vrrpInterface: eth0", "vrrpInterface: 'eth0 bad'", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := renderChart(t, bad); err == nil {
				t.Fatal("invalid VIP configuration accepted")
			}
		})
	}
}

// Helm compares host text; the supervisor checks parsed addresses. Keep
// both validation boundaries covered without restricting valid IPv6 syntax.
func TestChartIPv6VIPAddressValidation(t *testing.T) {
	for _, tt := range []struct {
		name, first, second string
		chartRejects        bool
		duplicate           bool
	}{
		{"same spelling with different prefixes", "2001:db8::1/64", "2001:db8::1/128", true, true},
		{"different case and compression", "2001:db8::1/64", "2001:DB8:0::1/128", false, true},
		{"expanded with leading zeros", "2001:db8::1/64", "2001:0db8:0000:0000:0000:0000:0000:0001/128", false, true},
		{"IPv4 embedded and hexadecimal", "::ffff:192.0.2.1/128", "::ffff:c000:201/128", false, true},
		{"distinct IPv6 addresses", "2001:db8::1/64", "2001:db8::2/64", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := fmt.Sprintf(`edge: {enabled: true}
vip:
  provider: keepalived
  keepalived:
    interface: eth1
    vrrpInterface: eth0
    vips:
      - {name: dns-a, address: %q, vrid: 53}
      - {name: dns-b, address: %q, vrid: 54}
`, tt.first, tt.second)
			out, err := renderChart(t, values)
			if tt.chartRejects {
				if err == nil || !strings.Contains(err.Error(), "addresses must be unique") {
					t.Fatalf("expected Helm duplicate-address error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := newRendered(t, out)
			pod := mapOf(dig(r.wantEdge(t, "DaemonSet"), "spec", "template"))
			args := list(r.container(t, pod, "keepalived")["args"])
			c := keepalived.Config{RouterID: "node-a", UnicastSrc: "2001:db8::10", Peers: []string{"2001:db8::11"}}
			for _, arg := range args {
				if spec, ok := strings.CutPrefix(str(arg), "--vip="); ok {
					v, err := keepalived.ParseVIP(spec)
					if err != nil {
						t.Fatal(err)
					}
					c.VIPs = append(c.VIPs, v)
				}
			}
			if len(c.VIPs) != 2 {
				t.Fatalf("expected two rendered VIPs, got %d", len(c.VIPs))
			}
			_, err = keepalived.Render(c)
			if tt.duplicate {
				if err == nil || !strings.Contains(err.Error(), "share address") {
					t.Fatalf("expected supervisor duplicate-address error, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("distinct IPv6 VIPs rejected: %v", err)
			}
		})
	}
}

func TestChartStandaloneEdgeStatusPermissions(t *testing.T) {
	out, err := renderChart(t, "mode: standalone\nedge: {enabled: true}\n")
	if err != nil {
		t.Fatal(err)
	}
	r := newRendered(t, out)
	r.wantObject(t, "Role", "kudurru-edge-status")
	r.wantObject(t, "RoleBinding", "kudurru-edge-status")
	if dig(r.pod(t), "spec", "automountServiceAccountToken") != true {
		t.Fatal("standalone edge discovery needs its ServiceAccount token")
	}
}

// The edge derives the internal API token and CA from the same keys as
// serve. Missing credentials silently disable zone polling and send local
// zones through the cached resolver pool.
func TestChartEdgeInternalCredentials(t *testing.T) {
	for _, mode := range []string{"deployment", "daemonset", "sidecar"} {
		for _, keys := range []struct {
			name     string
			values   string
			current  string
			previous string
		}{
			{name: "generated", current: "kudurru-encryption-key"},
			{name: "existing", values: "auth: {encryptionKeySecret: my-key}\n", current: "my-key"},
			{name: "rotation", values: "auth: {encryptionKeySecret: my-key, encryptionKeyPreviousSecret: my-old-key}\n", current: "my-key", previous: "my-old-key"},
		} {
			t.Run(mode+"/"+keys.name, func(t *testing.T) {
				out, err := renderChart(t, keys.values+fmt.Sprintf("edge: {enabled: true, mode: %s}\n", mode))
				if err != nil {
					t.Fatal(err)
				}
				r := newRendered(t, out)
				r.checkGeneric(t)
				pod := r.pod(t)
				if mode != "sidecar" {
					kind := "Deployment"
					if mode == "daemonset" {
						kind = "DaemonSet"
					}
					pod = mapOf(dig(r.wantEdge(t, kind), "spec", "template"))
				}
				c := r.container(t, pod, "dnsdist")
				if c == nil {
					t.Fatal("no dnsdist container")
				}
				checkEdgeKeyFile(t, pod, c, "KUDURRU_ENCRYPTION_KEY_FILE", "/etc/kudurru/secrets/encryption/key", keys.current)
				if keys.previous != "" {
					checkEdgeKeyFile(t, pod, c, "KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE", "/etc/kudurru/secrets/encryption-previous/key", keys.previous)
				} else {
					for _, e := range list(c["env"]) {
						if dig(e, "name") == "KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE" {
							t.Error("edge references a previous key when none is configured")
						}
					}
				}
			})
		}
	}
}

func checkEdgeKeyFile(t *testing.T, pod, c map[string]any, env, path, secret string) {
	t.Helper()
	foundEnv := false
	for _, e := range list(c["env"]) {
		if dig(e, "name") == env {
			foundEnv = true
			if dig(e, "value") != path {
				t.Errorf("%s = %v, want %s", env, dig(e, "value"), path)
			}
		}
	}
	if !foundEnv {
		t.Errorf("edge lacks %s; zone polling needs internal credentials", env)
	}
	for _, m := range list(c["volumeMounts"]) {
		if dig(m, "mountPath") != filepath.Dir(path) {
			continue
		}
		if dig(m, "readOnly") != true {
			t.Errorf("%s must be mounted read-only", path)
		}
		for _, v := range list(dig(pod, "spec", "volumes")) {
			if dig(v, "name") == dig(m, "name") {
				if dig(v, "secret", "secretName") != secret || dig(v, "secret", "defaultMode") != 0o440 {
					t.Errorf("%s must mount Secret %s with mode 0440: %v", path, secret, v)
				}
				return
			}
		}
		t.Fatalf("%s mount has no volume", path)
	}
	t.Errorf("edge lacks the Secret mount for %s", path)
}
