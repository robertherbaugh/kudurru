package importers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBlockyOfflineFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/blocky.yaml")
	if err != nil {
		t.Fatal(err)
	}
	req := OfflineRequest{Namespace: "dns", Nameservers: []string{"ns.example.test"}}
	p, err := PreviewBlocky(t.Context(), req, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "secret-") {
		t.Fatal("source secret leaked")
	}
	if p.Source.Type != "blocky" || p.Source.URL != "" || len(p.Objects) != 7 || len(p.Zones) != 1 {
		t.Fatalf("unexpected plan shape: %s", raw)
	}
	upstreams := p.Settings["upstreams"].(map[string]any)
	groups := upstreams["groups"].(map[string]any)
	if len(groups) != 3 || len(upstreams["conditional"].(map[string]string)) != 1 {
		t.Fatal("lost upstream groups/routes")
	}
	servers := groups["default"].(map[string]any)["servers"].([]string)
	if len(servers) != 2 || servers[1] != "tls://resolver.example.test:853" {
		t.Fatal("incorrect resolver conversion")
	}
	policies, records := 0, 0
	for _, doc := range p.Objects {
		spec := doc["spec"].(map[string]any)
		switch doc["kind"] {
		case "Policy":
			policies++
			if doc["metadata"].(map[string]any)["name"] == "default" {
				if spec["blockMode"] != "nxdomain" || len(spec["blockListRefs"].([]any)) != 1 {
					t.Fatal("default filtering")
				}
			} else {
				if spec["allowOnly"] != true || spec["upstreamGroup"] == "" {
					t.Fatal("exclusive allow mode or client resolver lost")
				}
			}
		case "DNSRecordSet":
			records++
			endpoints := spec["endpoints"].([]any)
			if len(endpoints) != 4 {
				t.Fatal("apex/wildcard IPv4/IPv6 records lost")
			}
			for _, endpoint := range endpoints {
				if endpoint.(map[string]any)["recordTTL"] != float64(420) {
					t.Fatal("custom TTL lost")
				}
			}
		}
	}
	if policies != 2 || records != 1 {
		t.Fatal("missing policies/records")
	}
	again, err := PreviewBlocky(t.Context(), req, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(again)
	if string(raw) != string(other) {
		t.Fatal("unstable identities/order")
	}
}
func TestBlockyOfflineStrictAndLosses(t *testing.T) {
	for _, input := range []string{
		"upstreams: {groups: {default: '192.0.2.1'}}",
		"upstreams: {strategy: fastest}",
		"blocking: {clientGroupsBlock: {default: [missing]}}",
		"blocking: {denylists: {ads: []}, clientGroupsBlock: {default: [ads, ads]}}",
		"blocking: {clientGroupsBlock: {192.0.2.1: [], 192.0.2.1/32: []}}",
		"customDNS: {mapping: {Host.test: '192.0.2.1', host.test.: '192.0.2.2'}}",
		"conditional: {mapping: {Host.test: '192.0.2.1', host.test.: '192.0.2.2'}}",
		"customDNS: {mapping: {host.test: '192.0.2.1,192.0.2.1'}}",
	} {
		if _, err := PreviewBlocky(t.Context(), OfflineRequest{}, strings.NewReader(input)); err == nil {
			t.Fatalf("accepted malformed/ambiguous input: %s", input)
		}
	}
	input := `upstreams:
  groups:
    default: [https://user:secret-upstream-marker@resolver.test/dns-query]
blocking:
  denylists:
    skipped: [/secret-file-marker]
    scheduled:
      - |
        ads.test
  listSchedules: {scheduled: [work]}
  clientGroupsBlock: {default: [skipped, scheduled], '*.secret-client-marker': [skipped]}
customDNS:
  filterUnmappedTypes: false
  mapping: {host.test: 192.0.2.1}
conditional:
  fallbackUpstream: true
  mapping: {home.test: 192.0.2.2}
`
	p, err := PreviewBlocky(t.Context(), OfflineRequest{}, strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "secret-") {
		t.Fatal("source secrets leaked")
	}
	if len(p.Settings) != 0 || len(p.Zones) != 0 {
		t.Fatal("unsupported resolver/local semantics approximated")
	}
	for _, doc := range p.Objects {
		if doc["kind"] == "Policy" {
			spec := doc["spec"].(map[string]any)
			if _, ok := spec["blockListRefs"]; ok {
				t.Fatal("incomplete/scheduled filtering attached")
			}
		}
	}
	for _, part := range []string{"local file", "schedule", "hostname", "all simple mappings omitted", "all conditional routes omitted"} {
		if !strings.Contains(strings.Join(p.Warnings, " "), part) {
			t.Fatalf("missing loss %s", part)
		}
	}
}

func TestBlockyOfflineHostsAndNumericTTL(t *testing.T) {
	input := `blocking:
  denylists:
    ads:
      - |
        0.0.0.0 ads.test tracking.test
        *.suffix.test # original synthetic comment
  clientGroupsBlock: {default: [ads]}
customDNS:
  customTTL: 7
  mapping: {host.test: 192.0.2.1}
`
	p, err := PreviewBlocky(t.Context(), OfflineRequest{}, strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range p.Objects {
		spec := doc["spec"].(map[string]any)
		if doc["kind"] == "BlockList" {
			sources := spec["sources"].([]any)
			if len(sources[0].(map[string]any)["entries"].([]any)) != 3 {
				t.Fatal("hosts/wildcard lines not converted")
			}
		}
		if doc["kind"] == "DNSRecordSet" {
			for _, endpoint := range spec["endpoints"].([]any) {
				if endpoint.(map[string]any)["recordTTL"] != float64(420) {
					t.Fatal("numeric minutes not preserved")
				}
			}
		}
	}
}
