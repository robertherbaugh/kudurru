package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStaticClusterHostTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte("cluster:\n  peers:\n    - {name: node-a, address: '127.0.0.1:8082'}\n    - {name: node-b, address: '[2001:db8::2]:8082'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, []string{"KUDURRU_NODE__NAME=node-a", "KUDURRU_CLUSTER__ROLE=static"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Cluster.Static() || len(cfg.Cluster.Peers) != 2 || !slices.Contains(cfg.EnvLocked, "cluster.role") {
		t.Fatal("static host inputs lost")
	}
	for _, path := range []string{"cluster.role", "cluster.peers"} {
		key, ok := Lookup(path)
		if !ok || !key.HostOnly || !key.RestartRequired || key.Scope != ScopeNode || PublicClusterKey(key) {
			t.Fatal("topology became public settings authority", path)
		}
	}
	if _, _, err := ApplyClusterDocument(cfg, map[string]any{"cluster": map[string]any{"role": "auto"}}); err == nil {
		t.Fatal("replicated settings replaced installed topology")
	}
	for name, modify := range map[string]func(*Config){
		"unsupported role":    func(c *Config) { c.Cluster.Role = "follower" },
		"implicit membership": func(c *Config) { c.Cluster.Role = "auto" },
		"kube":                func(c *Config) { c.Store.Driver = "kube" },
		"webhook":             func(c *Config) { c.ExternalDNS.Webhook.Enabled = true },
		"missing self":        func(c *Config) { c.Node.Name = "node-c" },
		"empty":               func(c *Config) { c.Cluster.Peers = nil },
		"duplicate name":      func(c *Config) { c.Cluster.Peers[1].Name = "node-a" },
		"duplicate endpoint":  func(c *Config) { c.Cluster.Peers[1].Address = "127.0.0.1:08082" },
		"mapped duplicate":    func(c *Config) { c.Cluster.Peers[1].Address = "[::ffff:127.0.0.1]:8082" },
		"zero port":           func(c *Config) { c.Cluster.Peers[1].Address = "node-b:0" },
		"large port":          func(c *Config) { c.Cluster.Peers[1].Address = "node-b:65536" },
		"empty host":          func(c *Config) { c.Cluster.Peers[1].Address = ":8082" },
		"wildcard":            func(c *Config) { c.Cluster.Peers[1].Address = "0.0.0.0:8082" },
		"multicast":           func(c *Config) { c.Cluster.Peers[1].Address = "[ff02::1]:8082" },
		"url":                 func(c *Config) { c.Cluster.Peers[1].Address = "https://node-b:8082" },
		"userinfo":            func(c *Config) { c.Cluster.Peers[1].Address = "user@node-b:8082" },
		"path":                func(c *Config) { c.Cluster.Peers[1].Address = "node-b:8082/image" },
		"bad name":            func(c *Config) { c.Cluster.Peers[1].Name = "node B" },
		"too many":            func(c *Config) { c.Cluster.Peers = make([]StaticPeer, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			next := *cfg
			next.Cluster.Peers = slices.Clone(cfg.Cluster.Peers)
			modify(&next)
			if next.Validate() == nil {
				t.Fatal("invalid static topology admitted")
			}
		})
	}
}
