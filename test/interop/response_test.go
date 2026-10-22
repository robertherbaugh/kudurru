package interop

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

func TestResponseSynthesisThroughListeners(t *testing.T) {
	kudurrutest.RequireE2E(t)
	bind := namedBinary(t)
	dir := t.TempDir()
	const origin = "response-synthesis.test."
	writeFile(t, filepath.Join(dir, "zone"), fmt.Sprintf(`$ORIGIN %s
$TTL 300
@ IN SOA ns.%s hostmaster.%s 1 60 10 3600 30
@ IN NS ns
ns IN A 127.0.0.1
www IN A 192.0.2.80
private IN A 10.0.0.1
bogus IN A 203.0.113.66
`, origin, origin, origin))
	port := kudurrutest.FreePort(t)
	primary := startNamed(t, bind, dir, namedOptions(dir, port)+fmt.Sprintf("zone %q { type primary; file \"zone\"; };\n", origin), port)
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "response")
	cfg["upstreams"].(map[string]any)["groups"] = map[string]any{"default": map[string]any{"servers": []string{primary.addr}, "timeout": "2s"}}
	cfg["response"] = map[string]any{"dns64": map[string]any{"prefix": "64:ff9b::/96"}, "rebinding": map[string]any{"enabled": true}, "nxdomainIPv4": []string{"198.51.100.5"}, "bogusNXDOMAIN": []string{"203.0.113.66/32"}}
	cfg["objects"] = []map[string]any{
		{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": map[string]any{"name": "native", "namespace": "default"}, "spec": map[string]any{"clients": []map[string]any{{"ip": "127.0.0.2"}}}},
		{"apiVersion": "kudurru.io/v1alpha1", "kind": "Policy", "metadata": map[string]any{"name": "native", "namespace": "default"}, "spec": map[string]any{"clientGroupRefs": []string{"native"}, "response": map[string]any{"dns64Prefix": ""}, "rewrites": []map[string]any{{"name": "rewrite." + origin, "targets": []string{"192.0.2.99"}}}}},
	}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	query := func(name string, typ uint16, tcp, native, do bool) *dns.Msg {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name+origin, typ)
		q.SetEdns0(1232, do)
		c := &dns.Client{Timeout: 3 * time.Second}
		if tcp {
			c.Net = "tcp"
		}
		if native {
			c.Dialer = &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP("127.0.0.2")}}
			if tcp {
				c.Dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}
			}
		}
		m, _, err := c.ExchangeContext(t.Context(), q, spec.Ports.DNS)
		if err != nil {
			t.Fatal(err)
		}
		if m.AuthenticatedData {
			t.Fatal("synthetic answer asserted AD")
		}
		return m
	}
	for _, tcp := range []bool{false, true} {
		for range 2 {
			m := query("www.", dns.TypeAAAA, tcp, false, false)
			if m.Rcode != 0 || len(m.Answer) != 1 || m.Answer[0].(*dns.AAAA).AAAA.String() != "64:ff9b::c000:250" || m.Answer[0].Header().Ttl > 30 {
				t.Fatal(m)
			}
			m = query("www.", dns.TypeAAAA, tcp, true, false)
			if m.Rcode != 0 || len(m.Answer) != 0 {
				t.Fatal("client policy leaked through cache", m)
			}
		}
		if m := query("www.", dns.TypeAAAA, tcp, false, true); m.Rcode != 0 || len(m.Answer) != 0 {
			t.Fatal("DO denial changed", m)
		}
		if m := query("private.", dns.TypeA, tcp, false, false); m.Rcode != dns.RcodeRefused || !hasValidationEDE(m) {
			t.Fatal("private response passed", m)
		}
		if m := query("bogus.", dns.TypeA, tcp, false, false); m.Rcode != dns.RcodeNameError {
			t.Fatal("bogus negative overridden", m)
		}
		if m := query("missing.", dns.TypeA, tcp, false, false); !hasA(m.Answer, "missing."+origin, "198.51.100.5") {
			t.Fatal("negative replacement missing", m)
		}
		if m := query("rewrite.", dns.TypeA, tcp, true, false); !hasA(m.Answer, "rewrite."+origin, "192.0.2.99") {
			t.Fatal("policy rewrite missing", m)
		}
	}
}
