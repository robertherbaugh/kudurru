package interop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// TestBINDUpstream runs named as the primary of upstream.test and cond.test
// and forwards to it from kudurru: upstream.test through a forward DNSZone
// naming the "bind" upstream group, cond.test through upstreams.conditional.
func TestBINDUpstream(t *testing.T) {
	kudurrutest.RequireE2E(t)
	namedBin := namedBinary(t)
	dir := t.TempDir()
	namedPort := kudurrutest.FreePort(t)
	ndir := filepath.Join(dir, "named")
	if err := os.MkdirAll(ndir, 0o750); err != nil {
		t.Fatal(err)
	}
	conf := namedOptions(ndir, namedPort)
	for _, z := range []string{"upstream.test", "cond.test"} {
		data := fmt.Sprintf(`$TTL 300
@	IN SOA ns1.%[1]s. hostmaster.%[1]s. 2026101101 3600 900 1209600 300
@	IN NS ns1.%[1]s.
ns1	IN A 127.0.0.1
www	IN A 192.0.2.53
alias	IN CNAME www
`, z)
		if err := os.WriteFile(filepath.Join(ndir, z+".db"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		conf += fmt.Sprintf("zone %q { type primary; file %q; };\n", z, z+".db")
	}
	nd := startNamed(t, namedBin, ndir, conf, namedPort)
	if r, err := query(t, nd.addr, "www.upstream.test.", dns.TypeA, false); err != nil || !hasA(r.Answer, "www.upstream.test.", "192.0.2.53") {
		t.Fatalf("named primary not serving: %v %v", r, err)
	}

	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "forwarder")
	ups := cfg["upstreams"].(map[string]any)
	ups["groups"].(map[string]any)["bind"] = map[string]any{"servers": []any{nd.addr}, "strategy": "strict", "timeout": "2s"}
	ups["conditional"] = map[string]any{"cond.test.": "bind"}
	cfg["objects"] = []any{map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone",
		"metadata": map[string]any{"name": "upstream-test"},
		"spec": map[string]any{
			"zoneName": "upstream.test.", "type": "forward",
			"forward": map[string]any{"upstreamGroup": "bind"},
		},
	}}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})

	forwards := func(t *testing.T, zone string) {
		t.Helper()
		r, err := query(t, spec.Ports.DNS, "www."+zone, dns.TypeA, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.Rcode != dns.RcodeSuccess || !hasA(r.Answer, "www."+zone, "192.0.2.53") {
			t.Fatalf("kudurru did not forward www.%s to named: %v", zone, r)
		}
		r, err = query(t, spec.Ports.DNS, "alias."+zone, dns.TypeA, true)
		if err != nil || !hasA(r.Answer, "www."+zone, "192.0.2.53") {
			t.Fatalf("CNAME chain over TCP: %v %v", r, err)
		}
		r, err = query(t, spec.Ports.DNS, "missing."+zone, dns.TypeA, false)
		if err != nil || r.Rcode != dns.RcodeNameError {
			t.Fatalf("NXDOMAIN from named not passed through: %v %v", r, err)
		}
	}

	t.Run("ForwardZone", func(t *testing.T) {
		kudurrutest.Eventually(10*time.Second, 100*time.Millisecond, func() bool {
			r, err := query(t, spec.Ports.DNS, "www.upstream.test.", dns.TypeA, false)
			return err == nil && r.Rcode == dns.RcodeSuccess
		})
		forwards(t, "upstream.test.")
	})

	t.Run("Conditional", func(t *testing.T) {
		forwards(t, "cond.test.")
	})

	t.Run("DefaultGroupNotNamed", func(t *testing.T) {
		// A name outside both suffixes goes to the default group (the
		// discard port): named must not answer it.
		r, err := query(t, spec.Ports.DNS, "www.elsewhere.test.", dns.TypeA, false)
		if err == nil && r.Rcode == dns.RcodeSuccess && len(r.Answer) > 0 {
			t.Fatalf("www.elsewhere.test answered although its group is unreachable: %v", r)
		}
	})
}
