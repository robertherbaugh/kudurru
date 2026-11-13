package e2e

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// Client-group views (split horizon, D403) through the actual serve.go
// chain on the SQLite store: two ClientGroups get different answers for one
// name over UDP and TCP, a view-only name does not exist for other clients,
// a lan-only overlay name answered locally for lan never reaches the
// response cache that other clients share, publish-only data is never
// served, and AXFR carries the default data only.
func TestClientGroupViewsSplitHorizon(t *testing.T) {
	kudurrutest.RequireE2E(t)
	up := kudurrutest.StartUpstream(t, map[string]string{
		"printer.overlay.e2e.test.": "198.51.100.90",
	})
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "views")
	cfg["upstreams"].(map[string]any)["groups"] = map[string]any{
		"default": map[string]any{"servers": []string{up.Addr}, "timeout": "1s"},
	}
	object := func(kind, name string, spec map[string]any) map[string]any {
		meta := map[string]any{"name": name}
		if kind != "DNSZone" {
			meta["namespace"] = "default"
		}
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": kind, "metadata": meta, "spec": spec}
	}
	endpoint := func(name, typ, target, view string) map[string]any {
		e := map[string]any{"dnsName": name, "recordType": typ, "targets": []any{target}}
		if view != "" {
			e["kudurru"] = map[string]any{"view": view}
		}
		return e
	}
	cfg["objects"] = []any{
		object("DNSZone", "split", map[string]any{
			"zoneName": "split.e2e.test.", "type": "primary", "nameservers": []any{"ns1.split.e2e.test."},
			"policy": map[string]any{"zoneTransfer": "allow"},
		}),
		object("DNSZone", "overlay", map[string]any{
			"zoneName": "overlay.e2e.test.", "type": "primary", "nameservers": []any{"ns1.overlay.e2e.test."},
			"fallback": map[string]any{},
		}),
		object("ClientGroup", "lan", map[string]any{"clients": []any{map[string]any{"ip": "127.0.0.2"}}}),
		object("ClientGroup", "kids", map[string]any{"clients": []any{map[string]any{"ip": "127.0.0.3"}}}),
		object("DNSRecordSet", "split", map[string]any{"zoneRef": map[string]any{"name": "split"}, "endpoints": []any{
			endpoint("ns1.split.e2e.test", "A", "192.0.2.53", ""),
			endpoint("www.split.e2e.test", "A", "203.0.113.10", ""),
			endpoint("www.split.e2e.test", "A", "10.0.0.10", "lan"),
			endpoint("www.split.e2e.test", "A", "10.9.0.10", "kids"),
			endpoint("nas.split.e2e.test", "A", "10.0.0.20", "lan"),
			endpoint("tunnel.split.e2e.test", "A", "198.51.100.200", "public"),
		}}),
		object("DNSRecordSet", "overlay", map[string]any{"zoneRef": map[string]any{"name": "overlay"}, "endpoints": []any{
			endpoint("printer.overlay.e2e.test", "A", "10.0.0.40", "lan"),
		}}),
	}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: []string{"KUDURRU_ADMIN_PASSWORD=views-test-only-password"}})
	query := func(name, client, transport string) *dns.Msg {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
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
		return m
	}
	// The committed primary and the group index publish asynchronously.
	if !kudurrutest.Eventually(10*time.Second, 20*time.Millisecond, func() bool {
		return firstA(query("www.split.e2e.test.", "127.0.0.2", "udp")) == "10.0.0.10" &&
			firstA(query("www.split.e2e.test.", "127.0.0.1", "udp")) == "203.0.113.10"
	}) {
		t.Fatal("client-group views did not reach the served zone")
	}
	for _, transport := range []string{"udp", "tcp"} {
		for range 3 {
			for _, tc := range []struct {
				client, name string
				rcode        int
				want         string
			}{
				{"127.0.0.1", "www.split.e2e.test.", dns.RcodeSuccess, "203.0.113.10"},
				{"127.0.0.2", "www.split.e2e.test.", dns.RcodeSuccess, "10.0.0.10"},
				{"127.0.0.3", "www.split.e2e.test.", dns.RcodeSuccess, "10.9.0.10"},
				{"127.0.0.1", "nas.split.e2e.test.", dns.RcodeNameError, ""},
				{"127.0.0.3", "nas.split.e2e.test.", dns.RcodeNameError, ""},
				{"127.0.0.2", "nas.split.e2e.test.", dns.RcodeSuccess, "10.0.0.20"},
				{"127.0.0.2", "tunnel.split.e2e.test.", dns.RcodeNameError, ""},
				// The overlay name goes upstream (and into the shared cache)
				// for other clients; lan is answered locally every time.
				{"127.0.0.1", "printer.overlay.e2e.test.", dns.RcodeSuccess, "198.51.100.90"},
				{"127.0.0.2", "printer.overlay.e2e.test.", dns.RcodeSuccess, "10.0.0.40"},
				{"127.0.0.3", "printer.overlay.e2e.test.", dns.RcodeSuccess, "198.51.100.90"},
			} {
				m := query(tc.name, tc.client, transport)
				if m.Rcode != tc.rcode || firstA(m) != tc.want {
					t.Fatalf("%s %s from %s: rcode %d answer %q, want %d %q\n%v", transport, tc.name, tc.client, m.Rcode, firstA(m), tc.rcode, tc.want, m)
				}
				if tc.rcode == dns.RcodeNameError && (len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA) {
					t.Fatalf("%s %s from %s: negative answer without SOA: %v", transport, tc.name, tc.client, m)
				}
			}
		}
	}
	// AXFR from a lan client carries the default data only.
	tr := &dns.Transfer{DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second}
	axfr := new(dns.Msg)
	axfr.SetAxfr("split.e2e.test.")
	conn, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).DialContext(t.Context(), "tcp", spec.Ports.DNS)
	if err != nil {
		t.Fatal(err)
	}
	tr.Conn = &dns.Conn{Conn: conn}
	envelopes, err := tr.In(axfr, spec.Ports.DNS)
	if err != nil {
		t.Fatal(err)
	}
	var records []string
	for e := range envelopes {
		if e.Error != nil {
			t.Fatal(e.Error)
		}
		for _, rr := range e.RR {
			records = append(records, rr.String())
		}
	}
	all := strings.Join(records, "\n")
	if !strings.Contains(all, "203.0.113.10") {
		t.Fatalf("AXFR lost default data:\n%s", all)
	}
	for _, private := range []string{"10.0.0.", "10.9.0.", "nas.split", "tunnel"} {
		if strings.Contains(all, private) {
			t.Fatalf("AXFR leaked client-group or publish-only data %q:\n%s", private, all)
		}
	}
}
