package cloudflare

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdjustmentOverridesTranslationAndPublicViewIsolation(t *testing.T) {
	f := newFixture(t, public.CloudflareZone{Zone: "example.com", ZoneID: zoneID, Defaults: public.CloudflareDefaults{Proxied: true, TTL: 600, Comment: "human  whitespace"}, Translate: map[string]string{"10.0.0.0/24": "192.0.2.0/24", "fd00::/64": "2001:db8::/64"}})
	input := endpoint("10.0.0.5")
	input.RecordType = "a"
	input.Labels = map[string]string{"publish": "yes"}
	input.Kudurru = &public.EndpointExt{View: "public-only", ExpiresAt: &metav1.Time{Time: f.p.now().Add(1000000000)}}
	original := input.DeepCopy()
	got := f.adjusted(input)
	if got.Targets[0] != "192.0.2.5" || got.RecordTTL != 1 || got.Kudurru != nil || got.Labels != nil || !reflect.DeepEqual(input, *original) {
		t.Fatal("adjustment altered source/view", input, got)
	}
	input.RecordType = "AAAA"
	input.Targets = []string{"fd00::5"}
	got = f.adjusted(input)
	if got.Targets[0] != "2001:db8::5" {
		t.Fatal(got)
	}
	f.mu.Lock()
	f.planName = "Pro Website"
	f.mu.Unlock()
	if _, err := f.p.Destinations(f.ctx); err != nil {
		t.Fatal(err)
	}
	input = endpoint("10.0.0.5")
	input.ProviderSpecific = []public.ProviderSpecific{{Name: "cloudflare/targets", Value: `["PUBLIC.EXAMPLE.NET"]`}, {Name: "cloudflare/proxied", Value: "false"}, {Name: "cloudflare/ttl", Value: "60"}, {Name: "cloudflare/flatten-cname", Value: "true"}, {Name: "cloudflare/tags", Value: `["owner:team","env:test","env:test"]`}}
	got = f.adjusted(input)
	if got.RecordType != "CNAME" || got.Targets[0] != "public.example.net." || got.RecordTTL != 60 {
		t.Fatal(got)
	}
	for _, property := range got.ProviderSpecific {
		if property.Name == "cloudflare/targets" || property.Name == "cloudflare/ttl" {
			t.Fatal("adjustment-only property leaked")
		}
		if property.Name == "cloudflare/tags" && property.Value != `["env:test","owner:team"]` {
			t.Fatal(property)
		}
	}
	z, _ := f.p.destination(f.pub.Destination)
	post, err := f.p.post(got, got.Targets[0], z)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := post.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["comment"] != stampComment("human  whitespace", f.h.owner) || body["settings"].(map[string]any)["flatten_cname"] != true {
		t.Fatal(string(raw))
	}
}
func TestAdjustmentRejectsInvalidProviderOptionsBeforePublication(t *testing.T) {
	f := newFixture(t)
	for _, properties := range [][]public.ProviderSpecific{
		{{Name: "unknown", Value: "x"}}, {{Name: "cloudflare/proxied", Value: "invalid"}}, {{Name: "cloudflare/proxied", Value: ""}, {Name: "cloudflare/proxied", Value: "true"}},
		{{Name: "cloudflare/ttl", Value: "0"}}, {{Name: "cloudflare/ttl", Value: "30"}}, {{Name: "cloudflare/ttl", Value: "86401"}}, {{Name: "cloudflare/comment", Value: strings.Repeat("x", 33)}}, {{Name: "cloudflare/comment", Value: "kudurru:pretend"}},
		{{Name: "cloudflare/targets", Value: `[]`}}, {{Name: "cloudflare/targets", Value: `["not a name"]`}}, {{Name: "cloudflare/tags", Value: `["cf-reserved:value"]`}},
		{{Name: "cloudflare/ipv4-only", Value: "true"}}, {{Name: "cloudflare/flatten-cname", Value: "true"}}, {{Name: "cloudflare/proxied", Value: "true"}, {Name: "cloudflare/ipv4-only", Value: "true"}, {Name: "cloudflare/ipv6-only", Value: "true"}},
	} {
		e := endpoint("192.0.2.1")
		e.ProviderSpecific = properties
		if _, err := f.p.Adjust(f.ctx, f.pub, []public.Endpoint{e}); err == nil {
			t.Fatal("invalid property accepted", properties)
		}
	}
	e := public.Endpoint{DNSName: "www.example.com.", RecordType: "TXT", Targets: []string{`"text"`}, RecordTTL: 300, ProviderSpecific: []public.ProviderSpecific{{Name: "cloudflare/proxied", Value: "true"}}}
	if _, err := f.p.Adjust(f.ctx, f.pub, []public.Endpoint{e}); err == nil {
		t.Fatal("TXT proxy accepted")
	}
	e = endpoint("192.0.2.1")
	e.SetIdentifier = "weighted"
	if _, err := f.p.Adjust(f.ctx, f.pub, []public.Endpoint{e}); err == nil {
		t.Fatal("unsupported routing identity")
	}
}
func TestTranslationValidationIPv4IPv6AndOverlap(t *testing.T) {
	for _, in := range []map[string]string{{"10.0.0.1/24": "192.0.2.0/24"}, {"10.0.0.0/24": "2001:db8::/24"}, {"10.0.0.0/24": "192.0.2.0/25"}, {"10.0.0.0/24": "192.0.2.0/24", "10.0.0.0/25": "198.51.100.0/25"}, {"::ffff:10.0.0.0/120": "::ffff:192.0.2.0/120"}} {
		if _, err := translations(in); err == nil {
			t.Fatal("invalid prefixes accepted", in)
		}
	}
}
