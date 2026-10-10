package config

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// TestSchemaMatchesConfig is the drift test: every leaf of Config has a
// Schema entry and every Schema entry has a Config leaf.
func TestSchemaMatchesConfig(t *testing.T) {
	var leaves []string
	collect(reflect.TypeOf(Config{}), "", &leaves)
	sort.Strings(leaves)
	paths := Paths()
	if !reflect.DeepEqual(leaves, paths) {
		t.Fatalf("Config leaves and Schema differ:\nconfig: %v\nschema: %v", leaves, paths)
	}
}

func collect(t reflect.Type, prefix string, out *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("koanf")
		if tag == "-" || tag == "" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Duration(0)) {
			collect(f.Type, path, out)
			continue
		}
		*out = append(*out, path)
	}
}

func TestConfigShowKeysMatchYAML(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dataDir:", "proxyProtocolFrom:", "timeout: 2s"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshalled config should contain %q:\n%s", want, b)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.DataDir != "/var/lib/kudurru" || cfg.Listeners.API.Address != "0.0.0.0:8080" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if len(cfg.Listeners.DNS) != 1 || cfg.Listeners.DNS[0].Address != "0.0.0.0:53" {
		t.Fatalf("default DNS listener wrong: %+v", cfg.Listeners.DNS)
	}
	if g := cfg.Upstreams.Groups["default"]; g.Timeout.Std() != 2*time.Second || len(g.Servers) != 2 {
		t.Fatalf("default upstream group wrong: %+v", g)
	}
	if cfg.Node.Name == "" {
		t.Fatal("node name should default to the hostname")
	}
}

func TestLoadFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
node: { name: node-a, dataDir: /tmp/k }
listeners:
  dns: [{ address: "127.0.0.1:5353", protocols: [udp] }]
upstreams:
  groups:
    default: { servers: ["10.0.0.5"], strategy: strict, timeout: 1500ms }
    ad: { servers: ["10.0.0.6:53"] }
  conditional: { "corp.example.": ad }
log: { level: debug }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "name")
	if err := os.WriteFile(secret, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, []string{
		"KUDURRU_NODE__NAME_FILE=" + secret,
		"KUDURRU_LISTENERS__API__ADDRESS=127.0.0.1:18080",
		"KUDURRU_LISTENERS__PROXYPROTOCOLFROM=127.0.0.1/32, ::1/128",
		"KUDURRU_ADMIN_PASSWORD_FILE=/run/secrets/x", // reserved, ignored
		"PATH=/usr/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Name != "from-file" {
		t.Errorf("_FILE env override failed: %q", cfg.Node.Name)
	}
	if cfg.Listeners.API.Address != "127.0.0.1:18080" {
		t.Errorf("env override failed: %q", cfg.Listeners.API.Address)
	}
	if !reflect.DeepEqual(cfg.Listeners.ProxyProtocolFrom, []string{"127.0.0.1/32", "::1/128"}) {
		t.Errorf("list env override failed: %v", cfg.Listeners.ProxyProtocolFrom)
	}
	if cfg.Listeners.DNS[0].Address != "127.0.0.1:5353" || cfg.Upstreams.Groups["default"].Timeout.Std() != 1500*time.Millisecond {
		t.Errorf("file values lost: %+v", cfg)
	}
	if cfg.Upstreams.Groups["ad"].Servers[0] != "10.0.0.6:53" || cfg.Upstreams.Conditional["corp.example."] != "ad" {
		t.Errorf("maps lost: %+v", cfg.Upstreams)
	}
	sort.Strings(cfg.EnvLocked)
	if strings.Join(cfg.EnvLocked, ",") != "listeners.api.address,listeners.proxyProtocolFrom,node.name" {
		t.Errorf("EnvLocked wrong: %v", cfg.EnvLocked)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	if _, err := Load("", []string{"KUDURRU_NODE__BOGUS=1"}); err == nil {
		t.Error("unknown env key must be rejected")
	}
	if _, err := Load("", []string{"KUDURRU_UPSTREAMS__GROUPS=x"}); err == nil {
		t.Error("map keys from env must be rejected")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(path, []byte("upstreams: { groups: { default: { servers: [], strategy: fastest } }, conditional: { x.: nope } }\nlog: { level: loud }\n"), 0o600)
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("invalid config must be rejected")
	}
	for _, want := range []string{"servers is empty", "unknown strategy", "unknown group", "unknown level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}
