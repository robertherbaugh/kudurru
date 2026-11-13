package e2e

import (
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// Client-group views (split horizon, D403) through the actual serve.go
// chain on the SQLite store: two ClientGroups get different answers for one
// name over UDP and TCP, a view-only name does not exist for other clients,
// a lan-only overlay name answered locally for lan never reaches the
// response cache that other clients share, publish-only data is never
// served, and AXFR, IXFR and the mounted export carry the default data
// only: a view-only change advances the serial, and IXFR from the old
// serial returns the unchanged default data with the new serial.
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
	splitSet := cfg["objects"].([]any)[4].(map[string]any)
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
	transfer := func(m *dns.Msg) []dns.RR {
		t.Helper()
		tr := &dns.Transfer{DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second}
		conn, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).DialContext(t.Context(), "tcp", spec.Ports.DNS)
		if err != nil {
			t.Fatal(err)
		}
		tr.Conn = &dns.Conn{Conn: conn}
		envelopes, err := tr.In(m, spec.Ports.DNS)
		if err != nil {
			t.Fatal(err)
		}
		var records []dns.RR
		for e := range envelopes {
			if e.Error != nil {
				t.Fatal(e.Error)
			}
			records = append(records, e.RR...)
		}
		return records
	}
	private := []string{"10.0.0.", "10.9.0.", "nas.split", "tunnel"}
	noGroupData := func(what, all string) {
		t.Helper()
		if !strings.Contains(all, "203.0.113.10") {
			t.Fatalf("%s lost default data:\n%s", what, all)
		}
		for _, p := range private {
			if strings.Contains(all, p) {
				t.Fatalf("%s leaked client-group or publish-only data %q:\n%s", what, p, all)
			}
		}
	}
	axfr := new(dns.Msg)
	axfr.SetAxfr("split.e2e.test.")
	records := transfer(axfr)
	var lines []string
	for _, rr := range records {
		lines = append(lines, rr.String())
	}
	noGroupData("AXFR", strings.Join(lines, "\n"))
	before := records[0].(*dns.SOA).Serial

	// The mounted export route serves the default data only.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + spec.Ports.API + api.BasePath
	var session api.SessionInfo
	(&staticProcessClient{client: client, base: base}).request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "views-test-only-password"}, http.StatusOK, &session) // #nosec G101 -- disposable loopback test credential created by this fixture
	export := func() string {
		t.Helper()
		response, err := client.Get(base + "/zones/split/export")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("export: HTTP %d %v", response.StatusCode, err)
		}
		return string(body)
	}
	noGroupData("export", export())

	// A view-only change (lan's www moves) advances the serial, and
	// neither IXFR nor the export carries the new view data.
	splitSet["spec"].(map[string]any)["endpoints"].([]any)[2] = endpoint("www.split.e2e.test", "A", "10.0.0.11", "lan")
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	// While the changed zone is being committed the node withholds its
	// answer (fail-closed, D313/D332): wait without failing on a timeout.
	if !kudurrutest.Eventually(10*time.Second, 20*time.Millisecond, func() bool {
		q := new(dns.Msg)
		q.SetQuestion("www.split.e2e.test.", dns.TypeA)
		c := &dns.Client{Timeout: time.Second, Dialer: &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP("127.0.0.2")}}}
		m, _, err := c.ExchangeContext(t.Context(), q, spec.Ports.DNS)
		return err == nil && firstA(m) == "10.0.0.11"
	}) {
		t.Fatal("the view-only change did not reach the served zone")
	}
	if got := firstA(query("www.split.e2e.test.", "127.0.0.1", "udp")); got != "203.0.113.10" {
		t.Fatalf("a view-only change moved the default answer: %s", got)
	}
	ixfr := new(dns.Msg)
	ixfr.SetIxfr("split.e2e.test.", before, "ns1.split.e2e.test.", "hostmaster.split.e2e.test.")
	delta := transfer(ixfr)
	if len(delta) == 0 {
		t.Fatal("empty IXFR response")
	}
	after := delta[0].(*dns.SOA).Serial
	if after == before {
		t.Fatalf("a view-only change kept serial %d", before)
	}
	// The IXFR answer is the default zone with the new serial: in AXFR form
	// (this node keeps no IXFR journal for it, RFC 1995 §4) holding exactly
	// the default records the first AXFR held, and no group data.
	defaults := func(rrs []dns.RR) string {
		var out []string
		for _, rr := range rrs {
			if rr.Header().Rrtype != dns.TypeSOA {
				out = append(out, rr.String())
			}
		}
		return strings.Join(out, "\n")
	}
	if last, ok := delta[len(delta)-1].(*dns.SOA); !ok || last.Serial != after || defaults(delta) != defaults(records) {
		t.Fatalf("IXFR %d to %d is not the unchanged default data with the new serial:\n%v", before, after, delta)
	}
	var deltaLines []string
	for _, rr := range delta {
		deltaLines = append(deltaLines, rr.String())
	}
	noGroupData("IXFR after the view change", strings.Join(deltaLines, "\n"))
	if all := export(); strings.Contains(all, "10.0.0.11") {
		t.Fatalf("export leaked the changed view data:\n%s", all)
	} else {
		noGroupData("export after the view change", all)
	}
}
