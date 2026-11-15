package e2e

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// Exercise the actual serve.go dependency chain. A rewrite must reenter local
// authority and retain client policy while sharing only unmodified cache data.
func TestModifierTargetsAndClientCacheIsolation(t *testing.T) {
	kudurrutest.RequireE2E(t)
	up := kudurrutest.StartUpstream(t, map[string]string{
		"raw.modifiers.test.":    "198.51.100.81",
		"host.offline.e2e.test.": "198.51.100.66",
		"www.e2e.test.":          "198.51.100.67",
	})
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "modifiers")
	cfg["upstreams"].(map[string]any)["groups"] = map[string]any{
		"default": map[string]any{"servers": []string{up.Addr}, "timeout": "1s"},
	}
	objects := zoneObjects(map[string]string{"www": "192.0.2.80"})
	object := func(kind, name string, spec map[string]any) map[string]any {
		meta := map[string]any{"name": name}
		if kind != "DNSZone" {
			meta["namespace"] = "default"
		}
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": kind, "metadata": meta, "spec": spec}
	}
	objects = append(objects,
		object("DNSZone", "offline-child", map[string]any{"zoneName": "offline.e2e.test.", "type": "secondary", "secondary": map[string]any{"primaries": []string{"127.0.0.1:1"}}}),
		object("BlockList", "modifiers", map[string]any{"sources": []map[string]any{{"format": "adblock", "inline": []string{
			"||local.modifiers.test^$dnsrewrite=www.e2e.test",
			"||offline.modifiers.test^$dnsrewrite=host.offline.e2e.test",
			"||alias.modifiers.test^$dnsrewrite=raw.modifiers.test",
			"||raw.modifiers.test^$client=127.0.0.2,dnstype=A",
		}}}}),
		object("Policy", "modifiers", map[string]any{"clientGroupRefs": []string{"default"}, "blockListRefs": []string{"modifiers"}, "blockMode": "nxdomain", "priority": 100}),
	)
	cfg["objects"] = objects
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: []string{"KUDURRU_ADMIN_PASSWORD=modifier-test-only-password"}})
	query := func(name, client, transport string) *dns.Msg {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		q.SetEdns0(1232, true)
		c := &dns.Client{Net: transport, Timeout: 3 * time.Second, Dialer: &net.Dialer{}}
		if transport == "tcp" {
			c.Dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(client)}
		} else {
			c.Dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(client)}
		}
		m, _, err := c.ExchangeContext(t.Context(), q, spec.Ports.DNS)
		if err != nil {
			t.Fatal(err)
		}
		if m.AuthenticatedData {
			t.Fatal("rewritten answer asserted authenticity", m)
		}
		return m
	}
	// Startup readiness and the first object/list publication are asynchronous.
	if !kudurrutest.Eventually(5*time.Second, 20*time.Millisecond, func() bool {
		return firstA(query("local.modifiers.test.", "127.0.0.1", "udp")) == "192.0.2.80"
	}) {
		t.Fatal("rewrite did not reach local authority")
	}
	for _, transport := range []string{"udp", "tcp"} {
		if m := query("offline.modifiers.test.", "127.0.0.1", transport); m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
			t.Fatal("rewrite bypassed unavailable child authority", m)
		}
		if m := query("local.modifiers.test.", "127.0.0.1", transport); firstA(m) != "192.0.2.80" {
			t.Fatal("rewrite used recursive data for local authority", m)
		}
		for range 3 {
			if m := query("alias.modifiers.test.", "127.0.0.1", transport); firstA(m) != "198.51.100.81" {
				t.Fatal("permitted client lost target answer", m)
			}
			if m := query("alias.modifiers.test.", "127.0.0.2", transport); m.Rcode != dns.RcodeNameError || firstA(m) != "" {
				t.Fatal("cached rewrite bypassed target client policy", m)
			}
			if m := query("raw.modifiers.test.", "127.0.0.1", transport); firstA(m) != "198.51.100.81" {
				t.Fatal("client-specific block poisoned shared raw cache", m)
			}
		}
	}
}
