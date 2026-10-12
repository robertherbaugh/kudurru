package deploy_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

// Docker is not available to the tests, so the compose files are checked
// structurally: the subset of the Compose Specification they use, every
// reference resolves (volumes, secrets, network_mode, depends_on, bind
// sources), no published ports on services sharing a network namespace,
// every KUDURRU_* variable is real, and every mounted kudurru.yaml loads
// through config.Load with the service's environment.

var composeDirs = []string{
	"deploy/compose/single",
	"deploy/compose/single-edge",
	"deploy/compose/ha/node-a",
	"deploy/compose/ha/node-b",
	"deploy/compose/ha/node-c",
}

var composeTopLevel = []string{"name", "services", "volumes", "secrets", "networks", "configs"}

var composeServiceKeys = []string{
	"image", "command", "entrypoint", "environment", "ports", "volumes", "secrets",
	"healthcheck", "restart", "network_mode", "networks", "cap_add", "cap_drop",
	"depends_on", "profiles", "stop_grace_period", "read_only", "tmpfs",
	"security_opt", "sysctls", "user", "logging", "ulimits", "labels",
	"container_name", "hostname", "init",
}

var portRE = regexp.MustCompile(`^((\d{1,3}\.){3}\d{1,3}:)?\d{1,5}:\d{1,5}(/(udp|tcp))?$`)

func TestComposeFiles(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range composeDirs {
		t.Run(dir, func(t *testing.T) {
			abs := filepath.Join(root, dir)
			b, err := os.ReadFile(filepath.Join(abs, "docker-compose.yml")) //nolint:gosec // repository file
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := yamlv3.Unmarshal(b, &doc); err != nil {
				t.Fatalf("docker-compose.yml does not parse: %v", err)
			}
			for k := range doc {
				if !slices.Contains(composeTopLevel, k) && !strings.HasPrefix(k, "x-") {
					t.Errorf("unknown top-level key %q", k)
				}
			}
			services := mapOf(doc["services"])
			if services["kudurru"] == nil {
				t.Fatal("no kudurru service")
			}
			volumes := mapOf(doc["volumes"])
			secrets := mapOf(doc["secrets"])
			for name, s := range secrets {
				f := str(dig(s, "file"))
				if !strings.HasPrefix(f, "./secrets/") {
					t.Errorf("secret %s: file %q must live under ./secrets/ (created by the operator, never committed)", name, f)
				}
			}
			for name, sv := range services {
				checkComposeService(t, abs, name, mapOf(sv), services, volumes, secrets)
			}
		})
	}
}

func checkComposeService(t *testing.T, dir, name string, s, services, volumes, secrets map[string]any) {
	t.Helper()
	for k := range s {
		if !slices.Contains(composeServiceKeys, k) && !strings.HasPrefix(k, "x-") {
			t.Errorf("%s: unknown service key %q", name, k)
		}
	}
	if str(s["image"]) == "" {
		t.Errorf("%s: no image", name)
	}
	nm := str(s["network_mode"])
	if peer, ok := strings.CutPrefix(nm, "service:"); ok {
		ps := mapOf(services[peer])
		if ps == nil {
			t.Errorf("%s: network_mode names unknown service %s", name, peer)
		} else if strings.HasPrefix(str(ps["network_mode"]), "service:") {
			t.Errorf("%s: network_mode chains through %s", name, peer)
		}
		if !slices.Contains(list(s["depends_on"]), any(peer)) {
			t.Errorf("%s: shares %s's network namespace but does not depend on it", name, peer)
		}
	}
	if nm != "" && s["ports"] != nil {
		t.Errorf("%s: publishes ports while network_mode is %s (Compose rejects or ignores them)", name, nm)
	}
	if nm == "host" && s["sysctls"] != nil {
		t.Errorf("%s: sysctls cannot be set with network_mode host", name)
	}
	for _, p := range list(s["ports"]) {
		if !portRE.MatchString(str(p)) {
			t.Errorf("%s: port %q is not [ip:]host:container[/proto]", name, p)
		}
	}
	for _, d := range list(s["depends_on"]) {
		if services[str(d)] == nil {
			t.Errorf("%s: depends_on unknown service %v", name, d)
		}
	}
	for _, sec := range list(s["secrets"]) {
		if secrets[str(sec)] == nil {
			t.Errorf("%s: secret %v is not declared", name, sec)
		}
	}
	var configFile string
	for _, v := range list(s["volumes"]) {
		src, target, _ := strings.Cut(str(v), ":")
		target, _, _ = strings.Cut(target, ":")
		if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "/") {
			if !strings.HasPrefix(src, "./") {
				t.Errorf("%s: bind mount %s must be relative to the compose directory", name, src)
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, src)); err != nil {
				t.Errorf("%s: bind source %s: %v", name, src, err)
			}
			if target == "/etc/kudurru/kudurru.yaml" {
				configFile = filepath.Join(dir, src)
			}
		} else if _, ok := volumes[src]; !ok {
			t.Errorf("%s: volume %s is not declared", name, src)
		}
	}
	env := map[string]string{}
	for k, v := range mapOf(s["environment"]) {
		env[k] = str(v)
		if err := checkEnvName(k); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		// Every *_FILE variable names a secret, except the settings file the
		// binary writes (KUDURRU_SETTINGS_FILE, checked below).
		if strings.HasSuffix(k, "_FILE") && k != "KUDURRU_SETTINGS_FILE" && !strings.HasPrefix(str(v), "/run/secrets/") {
			t.Errorf("%s: %s should point at a Compose secret under /run/secrets", name, k)
		}
	}
	if name == "dnsdist" {
		backendEnv := mapOf(dig(services, "kudurru", "environment"))
		for _, key := range []string{"KUDURRU_ENCRYPTION_KEY_FILE", "KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE"} {
			want := str(backendEnv[key])
			if key == "KUDURRU_ENCRYPTION_KEY_FILE" && want == "" {
				t.Error("kudurru: no encryption key configured for the internal API")
			}
			if env[key] != want {
				t.Errorf("dnsdist: %s = %q, want the backend's %q for zone polling", key, env[key], want)
			}
			if want != "" {
				secret := strings.TrimPrefix(want, "/run/secrets/")
				if !slices.Contains(list(s["secrets"]), any(secret)) {
					t.Errorf("dnsdist: %s references unmounted secret %s", key, secret)
				}
			}
		}
	}
	if name == "kudurru" {
		if configFile == "" {
			t.Fatalf("kudurru: no ./kudurru.yaml mounted at /etc/kudurru/kudurru.yaml")
		}
		if test := list(dig(s, "healthcheck", "test")); !slices.Contains(test, any("--ready")) {
			t.Errorf("kudurru: healthcheck must probe readiness (kudurru health --ready)")
		}
		var settingsEnv []string
		for k, v := range env {
			if strings.Contains(k, "__") {
				settingsEnv = append(settingsEnv, k+"="+v)
			}
		}
		// UI settings edits persist only through the settings file (D106);
		// it must live on the data volume because the root fs is read-only.
		if sf := env["KUDURRU_SETTINGS_FILE"]; !strings.HasPrefix(sf, "/var/lib/kudurru/") {
			t.Errorf("kudurru: KUDURRU_SETTINGS_FILE = %q, want a path under /var/lib/kudurru", sf)
		}
		sort.Strings(settingsEnv)
		body, err := os.ReadFile(configFile) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		cfg := checkConfig(t, configFile, body, settingsEnv)
		if dnsdist := services["dnsdist"]; dnsdist != nil {
			// Behind dnsdist kudurru must not take :53 and must accept PROXYv2.
			for _, l := range cfg.Listeners.DNS {
				if strings.HasSuffix(l.Address, ":53") {
					t.Errorf("kudurru listens on %s while dnsdist owns :53", l.Address)
				}
			}
			if len(cfg.Listeners.ProxyProtocolFrom) == 0 {
				t.Errorf("kudurru behind dnsdist needs listeners.proxyProtocolFrom")
			}
		}
	}
}

// TestComposeHATwins enforces the ha-static contract: kudurru.yaml and
// objects.d are byte-identical on the three nodes, the compose files differ
// only in the node name, peers and priority index, and every dnsdist lists
// every node.
func TestComposeHATwins(t *testing.T) {
	root := repoRoot(t)
	nodes := []string{"node-a", "node-b", "node-c"}
	read := func(rel string) []byte {
		b, err := os.ReadFile(filepath.Join(root, "deploy/compose/ha", rel)) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, n := range nodes[1:] {
		if !bytes.Equal(read("node-a/kudurru.yaml"), read(n+"/kudurru.yaml")) {
			t.Errorf("%s/kudurru.yaml differs from node-a's (ha-static twins share one file)", n)
		}
		entriesA, _ := os.ReadDir(filepath.Join(root, "deploy/compose/ha/node-a/objects.d"))
		for _, e := range entriesA {
			if !bytes.Equal(read("node-a/objects.d/"+e.Name()), read(n+"/objects.d/"+e.Name())) {
				t.Errorf("%s/objects.d/%s differs from node-a's", n, e.Name())
			}
		}
	}
	allowed := regexp.MustCompile(`(node-[abc]|KUDURRU_NODE__NAME|--peers=|--priority-index=)`)
	base := strings.Split(string(read("node-a/docker-compose.yml")), "\n")
	for _, n := range nodes {
		lines := strings.Split(string(read(n+"/docker-compose.yml")), "\n")
		if len(lines) != len(base) {
			t.Errorf("%s/docker-compose.yml has %d lines, node-a %d", n, len(lines), len(base))
			continue
		}
		for i := range lines {
			if lines[i] != base[i] && !allowed.MatchString(lines[i]) {
				t.Errorf("%s/docker-compose.yml line %d differs beyond node identity: %q", n, i+1, lines[i])
			}
		}
		text := string(read(n + "/docker-compose.yml"))
		// Named backends: dnsdist's health check then verifies that the
		// answering kudurru is the node it expects (BackendID).
		if !strings.Contains(text, "--backends=node-a=10.0.0.11:5353,node-b=10.0.0.12:5353,node-c=10.0.0.13:5353") {
			t.Errorf("%s: dnsdist must list every kudurru node by name", n)
		}
		if !strings.Contains(text, "KUDURRU_NODE__NAME: "+n) {
			t.Errorf("%s: wrong node name", n)
		}
	}
}
