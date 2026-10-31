package importers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAdGuardOfflineFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/adguard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	req := OfflineRequest{Namespace: "dns", LocalZones: []string{"home.arpa"}, NameserverAddresses: []string{"192.0.2.99"}}
	p, err := PreviewAdGuard(t.Context(), req, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	for _, secret := range []string{"secret-auth-marker", "secret-key-marker", "secret-url-marker", "secret-rule-marker", "/secret-key-marker"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("source secret leaked")
		}
	}
	if p.Source.Type != "adguard" || p.Source.URL != "" || len(p.Objects) != 7 || len(p.Zones) != 1 {
		t.Fatalf("unexpected plan shape: %s", raw)
	}
	upstreams := p.Settings["upstreams"].(map[string]any)
	groups := upstreams["groups"].(map[string]any)
	if len(groups) != 2 || groups["default"].(map[string]any)["strategy"] != "strict" {
		t.Fatal("lost upstream groups")
	}
	policies := 0
	records := 0
	for _, doc := range p.Objects {
		spec := doc["spec"].(map[string]any)
		switch doc["kind"] {
		case "Policy":
			policies++
			if doc["metadata"].(map[string]any)["name"] == "default" {
				if spec["blockMode"] != "nxdomain" || len(spec["blockListRefs"].([]any)) != 1 {
					t.Fatal("wrong default filtering")
				}
			} else {
				if _, ok := spec["blockListRefs"]; ok {
					t.Fatal("disabled client inherited filtering")
				}
				if spec["queryLog"] != false {
					t.Fatal("client log privacy lost")
				}
			}
		case "DNSRecordSet":
			records++
			if len(spec["endpoints"].([]any)) != 3 {
				t.Fatal("local IP/CNAME or generated NS record lost")
			}
		}
	}
	if policies != 2 || records != 1 {
		t.Fatal("missing policy or records")
	}
	again, err := PreviewAdGuard(t.Context(), req, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(again)
	if string(raw) != string(other) {
		t.Fatal("preview is not deterministic")
	}
	warnings := strings.Join(p.Warnings, " ")
	for _, part := range []string{"unsupported settings", "user_rules", "wildcard", "authoritative", "not inspected offline"} {
		if !strings.Contains(warnings, part) {
			t.Fatalf("missing loss %s", part)
		}
	}
}
func TestAdGuardOfflineStrictAndPartial(t *testing.T) {
	for _, input := range []string{
		"filtering: {filtering_enabled: yes}",
		"dns: {filtering_enabled: true}\nfiltering: {filtering_enabled: false}",
		"filters: [{id: 1, url: 'https://one.test/list'}, {id: 1, url: 'https://two.test/list'}]",
		"clients: [{name: a, ids: [192.0.2.1]}, {name: b, ids: [192.0.2.1/32]}]",
		"clients: [{name: a}, {name: a}]",
		"filtering: {blocked_response_ttl: -1}",
		"filtering: {rewrites: [{domain: X.test, answer: 192.0.2.1}, {domain: x.test., answer: 192.0.2.2}]}",
	} {
		p, err := PreviewAdGuard(t.Context(), OfflineRequest{}, strings.NewReader(input))
		if err == nil {
			t.Fatalf("accepted malformed/ambiguous input: %s %+v", input, p)
		}
	}
	p, err := PreviewAdGuard(t.Context(), OfflineRequest{}, strings.NewReader("dns: {upstream_dns: [192.0.2.1, 192.0.2.2], upstream_mode: fastest_addr}\nfiltering: {rewrites: [{domain: host.test, answer: 192.0.2.3}]}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Settings) != 0 || len(p.Zones) != 0 || !strings.Contains(strings.Join(p.Warnings, " "), "entire upstream group is omitted") {
		t.Fatal("unsupported upstream/local semantics silently approximated")
	}
}
