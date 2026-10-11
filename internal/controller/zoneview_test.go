package controller

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestBuildZoneView(t *testing.T) {
	z := zoneObj("example", "Example.COM.")
	disabled := ep("off.example.com", "A", "192.0.2.9")
	disabled.Kudurru = &v1alpha1.EndpointExt{Disabled: true}
	ttl := ep("ttl.example.com", "A", "192.0.2.10")
	ttl.RecordTTL = 60
	eu := ep("geo.example.com", "A", "192.0.2.20")
	eu.SetIdentifier = "eu"
	us := ep("geo.example.com", "A", "192.0.2.21")
	us.SetIdentifier = "us"

	type want struct {
		key        string // ns/name
		blocked    string
		outside    int
		duplicates int
		kept       int
		names      int
	}
	tests := []struct {
		name    string
		zone    *v1alpha1.DNSZone
		rrsets  []*v1alpha1.DNSRecordSet
		records []RecordTuple
		want    []want
	}{
		{
			name: "served, names canonicalised, default and explicit TTL",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("apps", "web", "example", 0, ep("WWW.example.com", "a", "192.0.2.2", "192.0.2.1"), ttl),
			},
			records: []RecordTuple{
				{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.2", "192.0.2.1"}},
				{Name: "ttl.example.com.", Type: "A", TTL: 60, Targets: []string{"192.0.2.10"}},
			},
			want: []want{{key: "apps/web", kept: 2, names: 2}},
		},
		{
			name: "outside the zone is dropped; apex and suffix lookalikes",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("apps", "mixed", "example", 0,
					ep("example.com.", "TXT", "apex"),
					ep("notexample.com", "A", "192.0.2.3"),
					ep("www.other.org", "A", "192.0.2.4")),
			},
			records: []RecordTuple{{Name: "example.com.", Type: "TXT", TTL: 300, Targets: []string{"apex"}}},
			want:    []want{{key: "apps/mixed", outside: 2, kept: 1, names: 3}},
		},
		{
			name: "later creationTimestamp loses the identity",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("b", "later", "example", time.Minute, ep("www.example.com", "A", "192.0.2.99")),
				rrsetObj("a", "earlier", "example", 0, ep("www.example.com", "A", "192.0.2.1")),
			},
			records: []RecordTuple{{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}}},
			want: []want{
				{key: "a/earlier", kept: 1, names: 1},
				{key: "b/later", duplicates: 1, names: 1},
			},
		},
		{
			name: "equal timestamps: namespace then name breaks the tie",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("b", "x", "example", 0, ep("www.example.com", "A", "192.0.2.2")),
				rrsetObj("a", "y", "example", 0, ep("www.example.com", "A", "192.0.2.1")),
			},
			records: []RecordTuple{{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}}},
			want:    []want{{key: "a/y", kept: 1, names: 1}, {key: "b/x", duplicates: 1, names: 1}},
		},
		{
			name: "different type or set identifier is not a duplicate",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("a", "v4", "example", 0, ep("www.example.com", "A", "192.0.2.1"), eu),
				rrsetObj("a", "v6", "example", time.Second, ep("www.example.com", "AAAA", "2001:db8::1"), us),
			},
			records: []RecordTuple{
				{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}},
				{Name: "geo.example.com.", Type: "A", SetIdentifier: "eu", TTL: 300, Targets: []string{"192.0.2.20"}},
				{Name: "www.example.com.", Type: "AAAA", TTL: 300, Targets: []string{"2001:db8::1"}},
				{Name: "geo.example.com.", Type: "A", SetIdentifier: "us", TTL: 300, Targets: []string{"192.0.2.21"}},
			},
			want: []want{{key: "a/v4", kept: 2, names: 2}, {key: "a/v6", kept: 2, names: 2}},
		},
		{
			name: "disabled endpoints keep their identity but are not served",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("a", "owner", "example", 0, disabled),
				rrsetObj("a", "taker", "example", time.Second, ep("off.example.com", "A", "192.0.2.50")),
			},
			want: []want{{key: "a/owner", kept: 1, names: 1}, {key: "a/taker", duplicates: 1, names: 1}},
		},
		{
			name: "invalid record sets are excluded and claim nothing",
			zone: z,
			rrsets: []*v1alpha1.DNSRecordSet{
				rrsetObj("a", "bad", "example", 0, ep("www.example.com", "A", "not-an-ip")),
				rrsetObj("a", "dup", "example", 0, ep("x.example.com", "A", "192.0.2.1"), ep("X.example.com.", "A", "192.0.2.2")),
				rrsetObj("a", "good", "example", time.Second, ep("www.example.com", "A", "192.0.2.1")),
			},
			records: []RecordTuple{{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}}},
			want: []want{
				{key: "a/bad", blocked: v1alpha1.ReasonInvalid, names: 1},
				{key: "a/dup", blocked: v1alpha1.ReasonInvalid, names: 1},
				{key: "a/good", kept: 1, names: 1},
			},
		},
		{
			name:   "no zone",
			zone:   nil,
			rrsets: []*v1alpha1.DNSRecordSet{rrsetObj("a", "orphan", "missing", 0, ep("www.example.com", "A", "192.0.2.1"))},
			want:   []want{{key: "a/orphan", blocked: ReasonZoneNotFound, names: 1}},
		},
		{
			name: "recordSetSelector namespaces and labels",
			zone: func() *v1alpha1.DNSZone {
				zz := zoneObj("example", "example.com.")
				zz.Spec.RecordSetSelector = &v1alpha1.RecordSetSelector{
					Namespaces:    []string{"apps"},
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"dns": "public"}},
				}
				return zz
			}(),
			rrsets: func() []*v1alpha1.DNSRecordSet {
				in := rrsetObj("apps", "in", "example", 0, ep("a.example.com", "A", "192.0.2.1"))
				in.Labels = map[string]string{"dns": "public"}
				nolabel := rrsetObj("apps", "nolabel", "example", 0, ep("b.example.com", "A", "192.0.2.2"))
				otherNS := rrsetObj("other", "ns", "example", 0, ep("c.example.com", "A", "192.0.2.3"))
				otherNS.Labels = map[string]string{"dns": "public"}
				return []*v1alpha1.DNSRecordSet{in, nolabel, otherNS}
			}(),
			records: []RecordTuple{{Name: "a.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}}},
			want: []want{
				{key: "apps/in", kept: 1, names: 1},
				{key: "apps/nolabel", blocked: ReasonNotSelected, names: 1},
				{key: "other/ns", blocked: ReasonNotSelected, names: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := buildZoneView(tt.zone, tt.rrsets, nil)
			if !reflect.DeepEqual(v.Records, tt.records) {
				t.Errorf("records\n got %+v\nwant %+v", v.Records, tt.records)
			}
			if v.Hash != ContentHash(tt.records) {
				t.Errorf("hash %s does not match the expected records", v.Hash)
			}
			if len(v.Verdicts) != len(tt.want) {
				t.Fatalf("%d verdicts, want %d", len(v.Verdicts), len(tt.want))
			}
			for _, w := range tt.want {
				ns, name, _ := strings.Cut(w.key, "/")
				got := v.Verdicts[store.Key{Kind: KindDNSRecordSet, Namespace: ns, Name: name}]
				if got == nil {
					t.Fatalf("no verdict for %s", w.key)
				}
				if got.Blocked != w.blocked || len(got.Outside) != w.outside || len(got.Duplicates) != w.duplicates || got.Kept != w.kept || got.Names != w.names {
					t.Errorf("%s: got blocked=%q outside=%v duplicates=%v kept=%d names=%d, want %+v",
						w.key, got.Blocked, got.Outside, got.Duplicates, got.Kept, got.Names, w)
				}
			}
		})
	}
}

func TestBuildZoneViewDoesNotMutateInput(t *testing.T) {
	a := rrsetObj("a", "2", "example", time.Second, ep("www.example.com", "A", "192.0.2.2", "192.0.2.1"))
	b := rrsetObj("a", "1", "example", 0, ep("www.example.com", "A", "192.0.2.1"))
	in := []*v1alpha1.DNSRecordSet{a, b}
	_ = buildZoneView(zoneObj("example", "example.com."), in, nil)
	if in[0] != a || in[1] != b || a.Spec.Endpoints[0].Targets[0] != "192.0.2.2" {
		t.Fatal("input reordered or mutated")
	}
}

func TestContentHash(t *testing.T) {
	base := []RecordTuple{
		{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1", "192.0.2.2"}},
		{Name: "example.com.", Type: "TXT", TTL: 300, Targets: []string{"v=spf1 -all"}},
	}
	h := ContentHash(base)
	// Golden: changing the encoding changes every zone's serial once;
	// update deliberately.
	const golden = "f07a80b66be3c7ca76f9c2a3cb43b2a00530ae113b5c85de91a2e14cc39c3594"
	if h != golden {
		t.Fatalf("golden hash changed: %s", h)
	}
	if got := ContentHash(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty hash %s", got)
	}
	reordered := []RecordTuple{
		{Name: "example.com.", Type: "TXT", TTL: 300, Targets: []string{"v=spf1 -all"}},
		{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.2", "192.0.2.1"}},
	}
	if ContentHash(reordered) != h {
		t.Fatal("hash depends on tuple or target order")
	}
	if base[0].Targets[0] != "192.0.2.1" {
		t.Fatal("ContentHash sorted its input")
	}
	mutations := map[string]func(r *RecordTuple){
		"name":   func(r *RecordTuple) { r.Name = "web.example.com." },
		"type":   func(r *RecordTuple) { r.Type = "AAAA" },
		"setid":  func(r *RecordTuple) { r.SetIdentifier = "eu" },
		"ttl":    func(r *RecordTuple) { r.TTL = 301 },
		"target": func(r *RecordTuple) { r.Targets = []string{"192.0.2.1"} },
	}
	for name, m := range mutations {
		cp := []RecordTuple{base[0], base[1]}
		cp[0].Targets = append([]string(nil), base[0].Targets...)
		m(&cp[0])
		if ContentHash(cp) == h {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
	// Field boundaries are unambiguous even with separators in values.
	x := ContentHash([]RecordTuple{{Name: "a.", Type: "TXT", Targets: []string{"b c"}}})
	y := ContentHash([]RecordTuple{{Name: "a.", Type: "TXT", Targets: []string{"b", "c"}}})
	if x == y {
		t.Fatal("ambiguous target encoding")
	}
}

func TestNextSerial(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name string
		prev uint32
		want uint32
	}{
		{"first stamp takes unix time", 0, 1_800_000_000},
		{"date-style serial above unix time increments", 2026101101, 2026101101 + 1},
		{"older serial jumps to now", 1_700_000_000, 1_800_000_000},
		{"same second still increments", 1_800_000_000, 1_800_000_001},
		{"ahead of the clock increments", 1_900_000_000, 1_900_000_001},
		{"wraps per RFC 1982", math.MaxUint32, 1_800_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NextSerial(tt.prev, now); got != tt.want {
				t.Fatalf("NextSerial(%d) = %d, want %d", tt.prev, got, tt.want)
			}
		})
	}
}

func TestValidateEndpoint(t *testing.T) {
	tests := []struct {
		ep   v1alpha1.Endpoint
		want string // substring of the error, "" = valid
	}{
		{ep("www.example.com", "A", "192.0.2.1"), ""},
		{ep("www.example.com.", "A", "192.0.2.1", "192.0.2.2"), ""},
		{ep("*.example.com", "A", "192.0.2.1"), ""},
		{ep("_sip._tcp.example.com", "SRV", "10 60 5060 sip.example.com."), ""},
		{ep("example.com", "MX", "10 mail.example.com"), ""},
		{ep("example.com", "MX", "0 ."), ""},
		{ep("example.com", "CAA", `0 issue "letsencrypt.org"`), ""},
		{ep("example.com", "TXT", "v=spf1 -all"), ""},
		{ep("www.example.com", "AAAA", "2001:db8::1"), ""},
		{ep("www.example.com", "CNAME", "web.example.net."), ""},
		{ep("www.example.com", "HTTPS", `1 . alpn="h2"`), ""},
		{ep("www.example.com", "A", "2001:db8::1"), "not an IPv4"},
		{ep("www.example.com", "AAAA", "192.0.2.1"), "not an IPv6"},
		{ep("www.example.com", "A", ""), "empty"},
		{ep("www.example.com", "CNAME", "a.example.", "b.example."), "exactly one target"},
		{ep("www.example.com", "CNAME", "bad name"), "invalid character"},
		{ep("bad..example.com", "A", "192.0.2.1"), "empty label"},
		{ep("www.*.example.com", "A", "192.0.2.1"), "invalid character"},
		{ep(strings.Repeat("a", 64)+".example.com", "A", "192.0.2.1"), "longer than 63"},
		{ep("example.com", "MX", "mail.example.com"), "preference exchange"},
		{ep("example.com", "MX", "70000 mail.example.com"), "16-bit"},
		{ep("example.com", "SRV", "1 2 3"), "priority weight port target"},
		{ep("example.com", "CAA", "256 issue x"), "8-bit"},
		{ep("example.com", "CAA", "0 is-sue x"), "alphanumeric"},
		{v1alpha1.Endpoint{DNSName: "www.example.com", RecordType: "A"}, "no targets"},
		{v1alpha1.Endpoint{DNSName: "www.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: -1}, "out of range"},
	}
	for _, tt := range tests {
		name := tt.ep.DNSName + "/" + tt.ep.RecordType + "/" + strings.Join(tt.ep.Targets, ",")
		t.Run(name, func(t *testing.T) {
			err := ValidateEndpoint(tt.ep)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
