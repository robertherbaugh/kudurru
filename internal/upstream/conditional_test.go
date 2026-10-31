package upstream

import (
	"reflect"
	"strings"
	"testing"
)

func TestConditionalKeys(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want []string
		err  string
	}{
		{"corp.example", []string{"corp.example."}, ""},
		{"Corp.Example.", []string{"corp.example."}, ""},
		{".", []string{"."}, ""},
		{"[/a.example/b.example/]", []string{"a.example.", "b.example."}, ""},
		{"[//a.example//]", []string{"a.example."}, ""},
		{"[/a.example/]1.1.1.1", nil, "unexpected"},
		{"[/]", nil, "want [/domain"},
		{"[//]", nil, "lists no domain"},
		{"", nil, "empty"},
		{"a..example", nil, "invalid"},
		{"a example", nil, "invalid"},
	} {
		got, err := conditionalKey(tc.key)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("conditionalKey(%q): err %v, want %q", tc.key, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("conditionalKey(%q) = %v, %v; want %v", tc.key, got, err, tc.want)
		}
	}
	groups := map[string]GroupConfig{"a": {}, "b": {}}
	if _, err := newSuffixMap(map[string]string{"x.": "a", "[/x/]": "b"}, groups); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("conflicting duplicate suffix: %v", err)
	}
	if _, err := newSuffixMap(map[string]string{"x.": "a", "[/x/y/]": "a"}, groups); err != nil {
		t.Fatalf("consistent duplicate suffix: %v", err)
	}
}

func TestImportDnsmasq(t *testing.T) {
	in := `
# dnsmasq
server=/corp.example/10.in-addr.arpa/10.0.0.5
server=/lab.corp.example/10.0.0.6#5353
server=/public.corp.example/#
server=9.9.9.9
# dnsproxy / AdGuard style
[/svc.example/]10.0.0.5
[/k8s.example/cluster.example/]tls://10.0.0.7 10.0.0.8:53
[/nofwd.example/]#
tls://1.1.1.1 https://dns.example/dns-query
`
	imp, err := ImportDnsmasq(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	wantGroups := map[string]GroupConfig{
		"dnsmasq-1": {Servers: []string{"10.0.0.5"}, Strategy: Strict},
		"dnsmasq-2": {Servers: []string{"10.0.0.6:5353"}, Strategy: Strict},
		"dnsmasq-3": {Servers: []string{"tls://10.0.0.7", "10.0.0.8:53"}, Strategy: Strict},
	}
	if !reflect.DeepEqual(imp.Groups, wantGroups) {
		t.Errorf("groups:\n got %v\nwant %v", imp.Groups, wantGroups)
	}
	wantCond := map[string]string{
		"corp.example.":        "dnsmasq-1",
		"10.in-addr.arpa.":     "dnsmasq-1",
		"lab.corp.example.":    "dnsmasq-2",
		"public.corp.example.": DefaultGroup,
		"svc.example.":         "dnsmasq-1",
		"k8s.example.":         "dnsmasq-3",
		"cluster.example.":     "dnsmasq-3",
		"nofwd.example.":       DefaultGroup,
	}
	if !reflect.DeepEqual(imp.Conditional, wantCond) {
		t.Errorf("conditional:\n got %v\nwant %v", imp.Conditional, wantCond)
	}
	if want := []string{"9.9.9.9", "tls://1.1.1.1", "https://dns.example/dns-query"}; !reflect.DeepEqual(imp.Default, want) {
		t.Errorf("default: %v", imp.Default)
	}

	var cfg Config
	if err := imp.Merge(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}
	s := newSet(t, cfg, Options{})
	if g, c, _ := s.Route("host.lab.corp.example.", ""); g != "dnsmasq-2" || !c {
		t.Fatalf("route: %s", g)
	}
	if g, _, _ := s.Route("x.public.corp.example.", ""); g != DefaultGroup {
		t.Fatalf("'#' route: %s", g)
	}
	if err := imp.Merge(&cfg); err == nil {
		t.Fatal("merging the same groups twice must fail")
	}

	for _, bad := range []string{"server=/corp.example", "[/corp.example 10.0.0.1"} {
		if _, err := ImportDnsmasq(strings.NewReader(bad)); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestImportUnsupportedDoesNotMerge(t *testing.T) {
	unsupported, err := ImportDnsmasq(strings.NewReader("server=/supported.example/192.0.2.1\nlocal=/home.arpa/\nserver=/local.lan/\n"))
	if err != nil || len(unsupported.Issues) != 2 {
		t.Fatal("local-answer directives were not reported", err)
	}
	var untouched Config
	if err := unsupported.Merge(&untouched); err == nil || untouched.Groups != nil || untouched.Conditional != nil {
		t.Fatal("lossy import changed destination")
	}
}
