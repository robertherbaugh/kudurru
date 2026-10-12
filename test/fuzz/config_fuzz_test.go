package fuzz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// configSeeds are kudurru.yaml documents the loader must handle.
var configSeeds = []string{
	"",
	"node: { name: a, dataDir: /tmp/k }\nlisteners:\n  dns: [{ address: \"127.0.0.1:5353\", protocols: [udp, tcp] }]\n",
	"upstreams:\n  groups:\n    default: { servers: [\"10.0.0.5\"], strategy: strict, timeout: 1500ms }\n    ad: { servers: [\"10.0.0.6:53\"] }\n  conditional: { \"corp.example.\": ad, \"[/lab/10.in-addr.arpa/]\": ad }\n",
	"cache: { maxEntries: 10, minTTL: 5s, maxTTL: 1s }\n",
	"querylog: { enabled: true, sink: sqlite, retention: { raw: 30m, maxDBBytes: 10GiB }, exclude: { domains: [\"/[/\"] } }\n",
	"tsigKeys: [{ name: k., algorithm: hmac-sha256, secretFile: /nope }, { name: K, algorithm: hmac-sha512, secretFile: /x }]\n",
	"ratelimit: { qpm: 100, ipv4Prefix: 33 }\nlisteners: { proxyProtocolFrom: [\"10.0.0.1\"] }\n",
	"log: { level: \"debug,dns=warn\" }\nnotify: { source: \"fe80::1%eth0\" }\n",
	"objects:\n  - { apiVersion: kudurru.io/v1alpha1, kind: DNSZone, metadata: { name: x }, spec: { zoneName: x., type: primary } }\n",
	"listeners: { dns: !!binary aGVsbG8= }\n",
	"a: &a [*a]\n",
}

// FuzzConfigLoad loads arbitrary YAML as kudurru.yaml. Load may refuse it
// but must not panic, and an accepted configuration must validate again
// and keep the invariants Validate promises.
func FuzzConfigLoad(f *testing.F) {
	for _, s := range configSeeds {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "kudurru.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path, nil)
		if err != nil {
			return
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Load accepted a configuration that Validate refuses: %v", err)
		}
		if _, ok := cfg.Upstreams.Groups["default"]; !ok || len(cfg.Listeners.DNS) == 0 {
			t.Fatalf("accepted configuration lacks the default group or a DNS listener: %+v", cfg)
		}
		// What the server builds from the accepted settings must not panic.
		if _, err := upstream.New(upstream.ConfigFrom(cfg.Upstreams), upstream.Options{}); err == nil {
			_ = cfg.BlockingListsDir()
		}
	})
}

// FuzzConfigEnv sets one KUDURRU_<KEY>=value override.
func FuzzConfigEnv(f *testing.F) {
	f.Add("NODE__NAME", "node-a")
	f.Add("LISTENERS__PROXYPROTOCOLFROM", "127.0.0.1/32, ::1/128")
	f.Add("CACHE__MAXENTRIES", "-1")
	f.Add("QUERYLOG__ENABLED", "maybe")
	f.Add("UPSTREAMS__GROUPS", "x")
	f.Add("BLOCKING__REFRESHINTERVAL", "1h")
	f.Add("QUERYLOG__RETENTION__MAXDBBYTES", "9999999999TiB")
	f.Add("NODE__NAME_FILE", "/dev/null")
	f.Fuzz(func(t *testing.T, key, value string) {
		if strings.HasSuffix(strings.ToUpper(key), "_FILE") {
			return // reads arbitrary files
		}
		cfg, err := config.Load("", []string{config.EnvPrefix + key + "=" + value})
		if err != nil {
			return
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Load accepted an environment that Validate refuses: %v", err)
		}
	})
}

// FuzzConfigValues round-trips the Duration and ByteSize value types the
// settings API reads and writes as text.
func FuzzConfigValues(f *testing.F) {
	for _, s := range []string{"2s", "1h30m", "-5m", "0", "10GiB", "1KB", "512", "9223372036854775807", "1TiB", " 3 MiB ", "1e3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var d config.Duration
		if err := d.UnmarshalText([]byte(s)); err == nil {
			txt, err := d.MarshalText()
			if err != nil {
				t.Fatal(err)
			}
			var d2 config.Duration
			if err := d2.UnmarshalText(txt); err != nil || d2 != d {
				t.Fatalf("Duration %q -> %q -> %v (%v)", s, txt, d2, err)
			}
		}
		var b config.ByteSize
		if err := b.UnmarshalText([]byte(s)); err == nil {
			txt, err := b.MarshalText()
			if err != nil {
				t.Fatal(err)
			}
			var b2 config.ByteSize
			if err := b2.UnmarshalText(txt); err != nil || b2 != b {
				t.Fatalf("ByteSize %q = %d -> %q -> %d (%v)", s, int64(b), txt, int64(b2), err)
			}
		}
	})
}

// FuzzDnsmasqImport feeds dnsmasq configuration to the conditional
// forwarding import (`upstreams.importFile`). Whatever imports must build
// an upstream set.
func FuzzDnsmasqImport(f *testing.F) {
	f.Add("server=/corp.example/10.0.0.53\nserver=/lab.corp.example/10.1.0.53#5353\nserver=1.1.1.1\n")
	f.Add("server=/10.in-addr.arpa/192.168.1.1\nserver=/public.corp.example/#\n")
	f.Add("server=/a/b/c/[2001:db8::53]:53\nrev-server=10.0.0.0/8,10.0.0.1\n")
	f.Add("# comment\nlocal=/home/\naddress=/x/1.2.3.4\nserver=/x/\n")
	f.Fuzz(func(t *testing.T, conf string) {
		imp, err := upstream.ImportDnsmasq(strings.NewReader(conf))
		if err != nil {
			return
		}
		// The servers are taken as written (validated when the set is
		// built); replace them by loopback so that only the imported
		// suffixes and group names are under test and nothing dials out.
		groups := map[string]upstream.GroupConfig{upstream.DefaultGroup: {Servers: []string{"127.0.0.1:53"}}}
		for name, g := range imp.Groups {
			if len(g.Servers) == 0 {
				t.Fatalf("group %s without servers from %q", name, conf)
			}
			groups[name] = upstream.GroupConfig{Servers: []string{"127.0.0.1:53"}, Strategy: g.Strategy}
		}
		set, err := upstream.New(upstream.Config{Groups: groups, Conditional: imp.Conditional}, upstream.Options{})
		if err != nil {
			t.Fatalf("imported configuration does not build: %v\n%s\n%+v", err, conf, imp)
		}
		defer func() { _ = set.Close() }()
		for suffix := range imp.Conditional {
			if _, _, err := set.Route("x."+strings.Trim(suffix, "[]/")+".", ""); err != nil {
				t.Fatalf("route for an imported suffix %q: %v", suffix, err)
			}
		}
	})
}
