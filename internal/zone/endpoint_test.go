package zone

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestParseEndpoint(t *testing.T) {
	path := field.NewPath("spec", "endpoints").Index(3)
	for _, tc := range []struct {
		name string
		ep   v1alpha1.Endpoint
		want []string // records, when valid
		errs []string // "field: substring"
	}{
		{name: "a", ep: ep("nas.home.example.com", "A", "10.0.0.20"),
			want: []string{"nas.home.example.com. 300 IN A 10.0.0.20"}},
		{name: "trailing-dot-and-case", ep: ep("NAS.home.example.com.", "AAAA", "2001:DB8::1"),
			want: []string{"nas.home.example.com. 300 IN AAAA 2001:db8::1"}},
		{name: "apex-at", ep: epTTL("@", "TXT", 60, "hello world"),
			want: []string{`home.example.com. 60 IN TXT "hello world"`}},
		{name: "txt-raw-quotes", ep: ep("t.home.example.com", "TXT", `say "hi" \o/`),
			want: []string{`t.home.example.com. 300 IN TXT "say \"hi\" \\o/"`}},
		{name: "txt-presentation", ep: ep("t.home.example.com", "TXT", `"a" "b"`),
			want: []string{`t.home.example.com. 300 IN TXT "a" "b"`}},
		{name: "cname-absolute-without-dot", ep: ep("w.home.example.com", "CNAME", "lb.example.net"),
			want: []string{"w.home.example.com. 300 IN CNAME lb.example.net."}},
		{name: "mx", ep: ep("home.example.com", "MX", "10 mail.example.net", "20 mail2.example.net."),
			want: []string{"home.example.com. 300 IN MX 10 mail.example.net.", "home.example.com. 300 IN MX 20 mail2.example.net."}},
		{name: "https", ep: ep("nas.home.example.com", "HTTPS", "1 . alpn=h2,h3"),
			want: []string{`nas.home.example.com. 300 IN HTTPS 1 . alpn="h2,h3"`}},
		{name: "rfc3597-known-type", ep: ep("x.home.example.com", "TYPE1", `\# 4 0a000001`),
			want: []string{"x.home.example.com. 300 IN A 10.0.0.1"}},
		{name: "rfc3597-unknown", ep: ep("x.home.example.com", "TYPE65534", `\# 3 abcdef`),
			want: []string{`x.home.example.com. 300 IN TYPE65534 \# 3 abcdef`}},
		{name: "rfc3597-empty", ep: ep("x.home.example.com", "TYPE65534", `\# 0`),
			want: []string{`x.home.example.com. 300 IN TYPE65534 \# 0`}},
		{name: "escaped-owner", ep: ep(`a\.b.home.example.com`, "A", "10.0.0.1"),
			want: []string{`a\.b.home.example.com. 300 IN A 10.0.0.1`}},
		{name: "escaped-space-owner", ep: ep(`sp\032ace.home.example.com`, "A", "10.0.0.1"),
			want: []string{`sp\ ace.home.example.com. 300 IN A 10.0.0.1`}},
		{name: "dollar-in-owner", ep: ep(`\$include.home.example.com`, "A", "10.0.0.1"),
			errs: []string{"spec.endpoints[3].dnsName: must not contain $"}},
		{name: "wildcard", ep: ep("*.apps.home.example.com", "A", "10.0.0.30"),
			want: []string{"*.apps.home.example.com. 300 IN A 10.0.0.30"}},

		{name: "outside-zone", ep: ep("nas.example.net", "A", "10.0.0.1"),
			errs: []string{"spec.endpoints[3].dnsName: outside zone"}},
		{name: "suffix-not-label", ep: ep("xhome.example.com", "A", "10.0.0.1"),
			errs: []string{"spec.endpoints[3].dnsName: outside zone"}},
		{name: "bad-name", ep: ep("a..home.example.com", "A", "10.0.0.1"),
			errs: []string{"spec.endpoints[3].dnsName"}},
		{name: "zone-file-syntax-in-name", ep: ep("a;b.home.example.com", "A", "10.0.0.1"),
			errs: []string{"spec.endpoints[3].dnsName: must be a domain name"}},
		{name: "unsupported-type", ep: ep("a.home.example.com", "HINFO", "x y"),
			errs: []string{"spec.endpoints[3].recordType: Unsupported value"}},
		{name: "soa-type", ep: ep("a.home.example.com", "SOA", "a. b. 1 2 3 4 5"),
			errs: []string{"spec.endpoints[3].recordType"}},
		{name: "opt-as-typennn", ep: ep("a.home.example.com", "TYPE41", `\# 0`),
			errs: []string{"spec.endpoints[3].recordType"}},
		{name: "typennn-range", ep: ep("a.home.example.com", "TYPE70000", `\# 0`),
			errs: []string{"spec.endpoints[3].recordType"}},
		{name: "bad-ttl", ep: epTTL("a.home.example.com", "A", -5, "10.0.0.1"),
			errs: []string{"spec.endpoints[3].recordTTL"}},
		{name: "no-targets", ep: ep("a.home.example.com", "A"),
			errs: []string{"spec.endpoints[3].targets: Required"}},
		{name: "bad-ip", ep: ep("a.home.example.com", "A", "10.0.0.1", "300.1.1.1"),
			errs: []string{"spec.endpoints[3].targets[1]: Invalid value"}},
		{name: "ipv6-in-a", ep: ep("a.home.example.com", "A", "2001:db8::1"),
			errs: []string{"spec.endpoints[3].targets[0]"}},
		{name: "empty-target", ep: ep("a.home.example.com", "A", "  "),
			errs: []string{"spec.endpoints[3].targets[0]: Invalid value: \"  \": must not be empty"}},
		{name: "garbage-after-rdata", ep: ep("a.home.example.com", "A", "10.0.0.1 10.0.0.2"),
			errs: []string{"spec.endpoints[3].targets[0]: Invalid value: \"10.0.0.1 10.0.0.2\": does not parse as A rdata: garbage after rdata"}},
		{name: "multi-line", ep: ep("a.home.example.com", "A", "10.0.0.1\nb.home.example.com. 300 IN A 10.0.0.2"),
			errs: []string{"spec.endpoints[3].targets[0]: Invalid value"}},
		{name: "two-cname-targets", ep: ep("a.home.example.com", "CNAME", "x.example.net", "y.example.net"),
			errs: []string{"spec.endpoints[3].targets: Too many"}},
		{name: "cname-root", ep: ep("a.home.example.com", "CNAME", "."),
			errs: []string{"spec.endpoints[3].targets[0]: Invalid value: \".\": a CNAME target cannot be the root"}},
		{name: "typennn-mismatch", ep: ep("a.home.example.com", "TYPE28", "10.0.0.1"),
			errs: []string{"spec.endpoints[3].targets[0]"}},
		{name: "rfc3597-wrong-length", ep: ep("a.home.example.com", "TYPE1", `\# 3 0a0000`),
			errs: []string{"spec.endpoints[3].targets[0]"}},
		{name: "rfc3597-no-presentation-form", ep: ep("a.home.example.com", "TYPE107", `\# 2 0000`),
			errs: []string{"spec.endpoints[3].targets[0]: Invalid value: \"\\\\# 2 0000\": invalid rdata: no presentation form that parses back"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.ep
			got, errs := ParseEndpoint("home.example.com.", 300, &e, path)
			if len(tc.errs) > 0 {
				if len(errs) != len(tc.errs) {
					t.Fatalf("errors %v, want %v", errs, tc.errs)
				}
				for i, want := range tc.errs {
					f, sub, _ := strings.Cut(want, ": ")
					if errs[i].Field != f || !strings.Contains(errs[i].Error(), sub) {
						t.Errorf("error %d = %q (field %s), want field %s containing %q", i, errs[i].Error(), errs[i].Field, f, sub)
					}
				}
				return
			}
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			checkSection(t, "records", got, tc.want)
		})
	}
}

func TestParseEndpointTypeList(t *testing.T) {
	// Every advertised type parses a representative target.
	samples := map[string]string{
		"A": "192.0.2.1", "AAAA": "2001:db8::1", "CAA": `0 iodef "mailto:x@example.com"`,
		"CNAME": "x.example.net", "DNAME": "example.net", "HTTPS": "1 . alpn=h3",
		"MX": "0 .", "NAPTR": `10 10 "S" "SIP+D2U" "" _sip._udp.example.net`, "NS": "ns.example.net",
		"PTR": "x.example.net", "SRV": "0 0 0 .", "SSHFP": "4 2 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"SVCB": "0 svc.example.net", "TLSA": "2 0 1 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"TXT": "x",
	}
	for _, typ := range SupportedTypes {
		target, ok := samples[typ]
		if !ok {
			t.Fatalf("no sample for %s", typ)
		}
		e := ep("x.example.org", typ, target)
		rrs, errs := ParseEndpoint("example.org.", 300, &e, field.NewPath("e"))
		if len(errs) > 0 || len(rrs) != 1 || dns.TypeToString[rrs[0].Header().Rrtype] != typ {
			t.Errorf("%s %q: %v %v", typ, target, rrs, errs)
		}
	}
}

func TestOwnerNameRoot(t *testing.T) {
	for in, want := range map[string]string{"@": ".", "com": "com.", "a.b.": "a.b."} {
		got, err := OwnerName(".", in)
		if err != nil || got != want {
			t.Errorf("OwnerName(., %q) = %q, %v", in, got, err)
		}
	}
}
