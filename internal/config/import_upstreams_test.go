package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDnsmasqTranslationAndBounds(t *testing.T) {
	parsed, err := ParseDnsmasq(strings.NewReader("server=/Corp.Example/192.0.2.1#5353\nserver=/corp.example/192.0.2.2\nserver=/public.corp.example/#\n[/svc.example/]tls://192.0.2.3 tcp://192.0.2.4\nserver=/./192.0.2.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Issues) != 0 || parsed.Conditional["public.corp.example."] != "default" || !reflect.DeepEqual(parsed.Groups[parsed.Conditional["corp.example."]].Servers, []string{"192.0.2.1:5353", "192.0.2.2"}) || parsed.Conditional["."] == "" {
		t.Fatal("translation lost suffix, exception or repeated servers")
	}
	for _, invalid := range []string{
		"server=/corp.example/192.0.2.1\nserver=/corp.example/#",
		"server=192.0.2.1@eth0", "server=ftp://private:secret@example.test",
		"server=", "server=/#/192.0.2.1", "server=//192.0.2.1",
		strings.Repeat("# comment\n", DnsmasqMaxBytes/10+2), strings.Repeat("#", 65537),
	} {
		p, err := ParseDnsmasq(strings.NewReader(invalid))
		if err == nil && (p == nil || len(p.Issues) == 0) {
			t.Fatal("unsupported input was not refused or reported")
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("diagnostic disclosed input credentials")
		}
	}
	var repeated strings.Builder
	for i := range 257 {
		_, _ = fmt.Fprintf(&repeated, "server=/example.test/192.0.2.1#%d\n", 1024+i)
	}
	if _, err := ParseDnsmasq(strings.NewReader(repeated.String())); err == nil {
		t.Fatal("unbounded suffix server set")
	}
	var domains, servers []string
	for i := range 1000 {
		domains = append(domains, fmt.Sprintf("d%d.example", i))
	}
	for i := range 128 {
		servers = append(servers, fmt.Sprintf("192.0.2.1:%d", 1024+i))
	}
	amplified := "[/" + strings.Join(domains, "/") + "/]" + strings.Join(servers, " ")
	if _, err := ParseDnsmasq(strings.NewReader(amplified)); err == nil {
		t.Fatal("small input expanded into unbounded routing")
	}

}

func TestLoadDnsmasqFilePrecedenceAndOwnership(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "node.yaml")
	put := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("node.yaml", "upstreams:\n  importFile: routes.conf\n")
	put("routes.conf", "server=192.0.2.1\nserver=/corp.example/192.0.2.2\nserver=/public.corp.example/#\n")
	cfg, err := Load(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstreams.ImportFile != "routes.conf" || !reflect.DeepEqual(cfg.Upstreams.Groups["default"].Servers, []string{"192.0.2.1"}) || len(cfg.Upstreams.Conditional) != 2 {
		t.Fatal("file was not expanded before canonical configuration")
	}
	seed := ClusterSeed(cfg)
	up := seed["upstreams"].(map[string]any)
	if _, exists := up["importFile"]; exists {
		t.Fatal("host path entered the public cluster document")
	}
	oldRevision := cfg.FileRevision
	put("routes.conf", "server=192.0.2.10\nserver=/corp.example/192.0.2.20\n")
	cfg, err = Load(source, []string{`KUDURRU_UPSTREAMS__BOOTSTRAP=192.0.2.99`})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FileRevision == oldRevision || cfg.Upstreams.Bootstrap[0] != "192.0.2.99" || cfg.Upstreams.Groups["default"].Servers[0] != "192.0.2.10" || len(cfg.EnvLocked) != 1 {
		t.Fatal("import revision or environment precedence lost")
	}
	// An explicitly UI-owned public section ignores a missing/stale import,
	// retaining only the host path. Releasing ownership must load it again.
	if err := os.Remove(filepath.Join(dir, "routes.conf")); err != nil {
		t.Fatal(err)
	}
	owned := func(*Config) (map[string]any, error) { return map[string]any{"upstreams": up}, nil }
	if cfg, err = LoadWithSections(source, nil, owned); err != nil || cfg.Upstreams.ImportFile != "routes.conf" {
		t.Fatal("UI ownership did not exclude the old file source", err)
	}
	if _, err := Load(source, nil); err == nil {
		t.Fatal("missing authoritative source accepted")
	}
	if _, err := Load(source, []string{"KUDURRU_UPSTREAMS__IMPORTFILE="}); err != nil {
		t.Fatal("environment could not disable source", err)
	}
}

func TestLoadDnsmasqRejectsWholeSource(t *testing.T) {
	for _, tc := range []struct{ name, node, routes string }{
		{"local", "", "server=/ok.example/192.0.2.1\nlocal=/home.arpa/"},
		{"directive", "", "address=/private.example/192.0.2.1"},
		{"group", "  groups: {dnsmasq-1: {servers: [192.0.2.9]}}\n", "server=/ok.example/192.0.2.1"},
		{"suffix", "  conditional: {'[/OK.EXAMPLE/]': default}\n", "server=/ok.example/192.0.2.1"},
		{"default", "  groups: {default: {servers: [192.0.2.9]}}\n", "server=192.0.2.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range map[string]string{"node.yaml": "upstreams:\n  importFile: routes.conf\n" + tc.node, "routes.conf": tc.routes} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if cfg, err := Load(filepath.Join(dir, "node.yaml"), nil); cfg != nil || err == nil || !strings.Contains(err.Error(), "upstreams.importFile") {
				t.Fatal("incomplete or colliding source admitted", err)
			}
		})
	}
}
