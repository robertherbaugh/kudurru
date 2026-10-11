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

func TestLoadTLSListenersAndProxySecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
listeners:
  dns:  [{ address: "0.0.0.0:5353", protocols: [udp, tcp] }]
  dot:  [{ address: "0.0.0.0:853", tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]
  doh:  [{ address: "0.0.0.0:443", path: /dns-query, tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]
  doq:  [{ address: "0.0.0.0:853", tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]
  doh3: [{ address: "0.0.0.0:443", tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]
  proxyProtocolFrom: ["10.0.0.11/32"]
  proxyProtocolSecretFile: /run/secrets/proxy
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Listeners
	if len(l.DoT) != 1 || l.DoT[0].Address != "0.0.0.0:853" || l.DoT[0].TLS.CertFile != "/certs/tls.crt" || l.DoT[0].TLS.KeyFile != "/certs/tls.key" {
		t.Errorf("dot: %+v", l.DoT)
	}
	if len(l.DoH) != 1 || l.DoH[0].Path != "/dns-query" || l.DoH[0].TLS.KeyFile != "/certs/tls.key" {
		t.Errorf("doh: %+v", l.DoH)
	}
	if len(l.DoQ) != 1 || len(l.DoH3) != 1 || l.DoH3[0].Path != "" {
		t.Errorf("doq/doh3: %+v %+v", l.DoQ, l.DoH3)
	}
	if l.ProxyProtocolSecretFile != "/run/secrets/proxy" {
		t.Errorf("secret file: %q", l.ProxyProtocolSecretFile)
	}

	// Defaults: no TLS listeners.
	def, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Listeners.DoT)+len(def.Listeners.DoH)+len(def.Listeners.DoQ)+len(def.Listeners.DoH3) != 0 || def.Listeners.ProxyProtocolSecretFile != "" {
		t.Errorf("TLS listeners must default to none: %+v", def.Listeners)
	}

	// The secret file path can come from the environment like any string key.
	env, err := Load("", []string{"KUDURRU_LISTENERS__PROXYPROTOCOLSECRETFILE=/run/secrets/env"})
	if err != nil {
		t.Fatal(err)
	}
	if env.Listeners.ProxyProtocolSecretFile != "/run/secrets/env" {
		t.Errorf("env override: %q", env.Listeners.ProxyProtocolSecretFile)
	}
}

func TestValidateTLSListenersAndProxySecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(path, []byte(`
listeners:
  dot:  [{ address: "", tls: { certFile: /c } }]
  doh:  [{ address: ":443", path: dns-query, tls: { certFile: /c, keyFile: /k } }]
  doq:  [{ address: ":853" }]
  doh3: [{ address: ":443", path: /dns-query, tls: { certFile: /c, keyFile: /k } }]
  proxyProtocolFrom: ["127.0.0.1/32", "10.0.0.11/32"]
`), 0o600)
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("invalid listeners must be rejected")
	}
	for _, want := range []string{
		"listeners.dot[0].address is required",
		"listeners.dot[0].tls: certFile and keyFile are required",
		`listeners.doh[0].path "dns-query" must start with /`,
		"listeners.doq[0].tls: certFile and keyFile are required",
		"proxyProtocolSecretFile is required when proxyProtocolFrom contains a non-loopback peer (10.0.0.11/32)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "doh3") {
		t.Errorf("valid doh3 listener reported: %v", err)
	}

	// Loopback-only peers need no secret (D56).
	ok := filepath.Join(dir, "ok.yaml")
	_ = os.WriteFile(ok, []byte("listeners: { proxyProtocolFrom: [\"127.0.0.1/32\", \"::1/128\"] }\n"), 0o600)
	if _, err := Load(ok, nil); err != nil {
		t.Fatalf("loopback-only proxyProtocolFrom must not need a secret: %v", err)
	}
}
