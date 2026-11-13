package importers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

var updateCoreDNS = flag.Bool("update-coredns", false, "rewrite testdata/coredns.golden.json")

func corednsPreview(t *testing.T, input string) *Plan {
	t.Helper()
	p, err := PreviewCoreDNS(t.Context(), OfflineRequest{Namespace: "dns"}, strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The fixture covers every converted plugin plus unsupported plugins, file
// references, encrypted listeners, placeholders and unresolved imports. The
// golden file pins the whole plan, including the ordered loss report.
func TestCoreDNSOfflineFixtureGolden(t *testing.T) {
	src, err := os.ReadFile("testdata/coredns.Corefile")
	if err != nil {
		t.Fatal(err)
	}
	p := corednsPreview(t, string(src))
	got, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if bytes.Contains(got, []byte("secret-")) || bytes.Contains(got, []byte("/etc/")) {
		t.Fatalf("source secret, placeholder or file path leaked:\n%s", got)
	}
	for i := 0; i < 3; i++ {
		again, _ := json.MarshalIndent(corednsPreview(t, string(src)), "", "  ")
		if !bytes.Equal(append(again, '\n'), got) {
			t.Fatal("conversion is not deterministic")
		}
	}
	const golden = "testdata/coredns.golden.json"
	if *updateCoreDNS {
		if err := os.WriteFile(golden, got, 0o600); err != nil { //nolint:gosec // G703: constant testdata path, written only with -update-coredns.
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("plan differs from %s (run with -update-coredns after review):\n%s", golden, got)
	}
}

func TestCoreDNSOfflineConversionShapes(t *testing.T) {
	src, _ := os.ReadFile("testdata/coredns.Corefile")
	p := corednsPreview(t, string(src))
	up := p.Settings["upstreams"].(map[string]any)
	groups := up["groups"].(map[string]any)
	def := groups["default"].(map[string]any)
	if s := def["servers"].([]string); len(s) != 3 || s[0] != "192.0.2.53" || s[1] != "tls://198.51.100.53" || s[2] != "[2001:db8::53]:5353" || def["strategy"] != "strict" {
		t.Fatalf("default forwarder: %v", def)
	}
	routes := up["conditional"].(map[string]string)
	if len(routes) != 2 || routes["home.example."] == "" || routes["home.example."] != routes["10.in-addr.arpa."] {
		t.Fatalf("conditional routes: %v", routes)
	}
	if s := groups[routes["home.example."]].(map[string]any)["servers"].([]string); len(s) != 1 || s[0] != "tcp://192.168.1.1" {
		t.Fatalf("force_tcp: %v", s)
	}
	if _, ok := routes["corp.example."]; ok || len(groups) != 2 {
		t.Fatal("TLS server name override was not omitted as a unit")
	}
	control := p.Settings["control"].(map[string]any)
	if n := control["allowedNetworks"].([]string); len(n) != 3 || n[0] != "10.0.0.7/32" {
		t.Fatalf("acl networks: %v", n)
	}
	if q := control["blockedQTypes"].([]string); len(q) != 2 || q[0] != "ANY" {
		t.Fatalf("acl types: %v", q)
	}
	cache := p.Settings["cache"].(map[string]any)
	if cache["maxTTL"] != "5m0s" || cache["negativeTTL"] != "1m0s" || cache["minTTL"] != "10s" || cache["maxEntries"] != 7000 {
		t.Fatalf("cache: %v", cache)
	}
	if len(p.Objects) != 1 || p.Objects[0]["kind"] != "Policy" {
		t.Fatalf("objects: %v", p.Objects)
	}
	rewrites := p.Objects[0]["spec"].(map[string]any)["rewrites"].([]any)
	// nas.lan, printer, tv CNAME and three PTR answers; nas.home.example is
	// served by the home.example block, so the root block never answered it.
	if len(rewrites) != 6 {
		t.Fatalf("rewrites: %v", rewrites)
	}
	report := strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"plugins not converted: errors, health, log, loop, prometheus, reload.",
		"1 unknown directives omitted (names redacted)",
		"file zones home.example are loaded from files that are not read",
		"import lines omitted",
		"forward with a TLS server name override",
		"forward to a resolv.conf-style file omitted",
		"plugins not converted: kubernetes.",
		"template uses an environment placeholder",
		"Server block tls://.:853: encrypted or gRPC listener block omitted",
		"Server block .:53: 1 hosts or rewrite names fall inside the more specific server block for home.example",
		"Server block ports are not imported",
		CorefileSyntax,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("loss report lacks %q", want)
		}
	}
}

func TestCoreDNSOfflineGrammar(t *testing.T) {
	// The braceless single-block form, quoted tokens and comment handling.
	p := corednsPreview(t, ". # root\nforward \"#not-a-comment\" 192.0.2.1\nhosts {\n  \"192.0.2.9\" quoted.example\n  192.0.2.8 \"a.example\" # trailing\n  no_reverse\n  fallthrough\n}\n")
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "quoted.example") || !strings.Contains(string(raw), "a.example") || strings.Contains(string(raw), "PTR") {
		t.Fatalf("quoted IP must not start an entry; no_reverse must suppress PTR: %s", raw)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "forward with an unsupported FROM zone") {
		t.Fatal("quoted # was not kept as a token")
	}
	// Snippets expand in place, recursion stays unresolved.
	p = corednsPreview(t, "(a) {\n import b\n}\n(b) {\n import a\n forward . 192.0.2.2\n}\n. {\n import a\n}\n")
	if p.Settings["upstreams"].(map[string]any)["groups"].(map[string]any)["default"] == nil {
		t.Fatal("snippet was not expanded")
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "1 import lines omitted") {
		t.Fatalf("recursive import: %v", p.Warnings)
	}
	for _, input := range []string{
		"",
		"# only a comment\n",
		". {\n forward . 192.0.2.1\n",
		". {\n}\n}\n",
		"{\n}\n",
		". {\n \"unterminated\n}\n",
		". {\n forward . 192.0.2.1\n}\nexample.org\n",
		". {\n}\n.:5353 {\n}\n",
		". {\n forward . 192.0.2.1\n forward . 192.0.2.2\n}\n",
		". {\n hosts {\n  192.0.2.1 a.example\n }\n rewrite name a.example b.example\n}\n",
		"(s) {\n}\n(s) {\n}\n. {\n}\n",
		". {\n {\n }\n}\n",
		"\xff",
		strings.Repeat("a ", maxCorefileTokens+1),
	} {
		if _, err := PreviewCoreDNS(t.Context(), OfflineRequest{}, strings.NewReader(input)); err == nil {
			t.Fatalf("accepted malformed or ambiguous Corefile: %q", input[:min(len(input), 60)])
		}
	}
}

func TestCoreDNSOfflineRefusalsAndRedaction(t *testing.T) {
	//nolint:gosec // G101: synthetic credential markers verify preview redaction.
	input := `. {
    forward . https://user:secret-userinfo@resolver.example/dns-query
    acl {
        allow type A net 192.0.2.0/24
        block
    }
    cache 30 secret-zone.example
    rewrite name regex (.*)\.secret-regex\.example {1}.other.example
    rewrite type ANY HINFO
    hosts {
        192.0.2.1 secret-outside-zone..example
        ttl 60
    }
}
`
	p := corednsPreview(t, input)
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "secret-") {
		t.Fatalf("leaked: %s", raw)
	}
	if len(p.Settings) != 0 || len(p.Objects) != 0 {
		t.Fatalf("unmappable input produced settings or objects: %s", raw)
	}
	report := strings.Join(p.Warnings, "\n")
	for _, want := range []string{"credential-bearing upstream", "all acl rules omitted", "cache scoped to zones", "rewrite name with prefix", "rewrite type is not converted", "hosts entries or options omitted", "hosts ttl", "SERVFAIL"} {
		if !strings.Contains(report, want) {
			t.Errorf("loss report lacks %q:\n%s", want, report)
		}
	}
}

func TestCoreDNSOfflineReadsNothingAndCancels(t *testing.T) {
	// Paths in the Corefile are reported, never opened: point them at files
	// that exist and hold markers, and the markers must not appear.
	dir := t.TempDir()
	for _, name := range []string{"hosts", "db.example", "snippet.conf", "resolv.conf"} {
		if err := os.WriteFile(dir+"/"+name, []byte("192.0.2.77 file-marker.example\nnameserver 192.0.2.78\nforward . 192.0.2.79\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := corednsPreview(t, ". {\n import "+dir+"/snippet.conf\n hosts "+dir+"/hosts\n file "+dir+"/db.example example.org\n}\nlab.example {\n forward . "+dir+"/resolv.conf\n}\n")
	raw, _ := json.Marshal(p)
	for _, marker := range []string{"file-marker", "192.0.2.7", dir} {
		if strings.Contains(string(raw), marker) {
			t.Fatalf("referenced file was read or its path echoed: %s", raw)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PreviewCoreDNS(ctx, OfflineRequest{}, strings.NewReader(". {\n}\n")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := PreviewCoreDNS(t.Context(), OfflineRequest{}, strings.NewReader(strings.Repeat(" ", maxOfflineBytes+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
}

// The hand-written reader must never panic, hang or echo a marker value
// from an unconverted position, whatever the input.
func FuzzCoreDNSPreview(f *testing.F) {
	src, _ := os.ReadFile("testdata/coredns.Corefile")
	for _, seed := range []string{string(src), ". {\n}\n", "(a) {\n import a\n}\n. {\n import a\n}\n", ". \"x\ny\" {\n}\n", ". {\n acl {\n  allow net *\n }\n}\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		p, err := PreviewCoreDNS(t.Context(), OfflineRequest{}, strings.NewReader(input))
		if err != nil {
			return
		}
		if _, err := json.Marshal(p); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCoreDNSOfflineSnippetExpansionBounded(t *testing.T) {
	body := strings.Repeat(" log\n", 2000)
	imports := strings.Repeat(" import big\n", 2000)
	input := "(big) {\n" + body + "}\n. {\n" + imports + "}\n"
	if _, err := PreviewCoreDNS(t.Context(), OfflineRequest{}, strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "200,000 directives") {
		t.Fatalf("snippet expansion was not bounded: %v", err)
	}
}

// Large inputs within the 4 MiB bound convert in bounded time.
func TestCoreDNSOfflineLargeInputsStayLinear(t *testing.T) {
	for name, input := range map[string]string{
		"acl types":     ". {\n acl {\n  block type " + strings.Repeat("A ", 400000) + "\n }\n}\n",
		"acl networks":  ". {\n acl {\n  allow net " + strings.Repeat("192.0.2.1 ", 200000) + "\n  block\n }\n}\n",
		"hosts aliases": ". {\n hosts {\n  192.0.2.1 " + strings.Repeat("a.example ", 199000) + "\n }\n}\n",
		"hosts lines":   ". {\n hosts {\n" + strings.Repeat("  192.0.2.1 a.example\n", 60000) + " }\n}\n",
		"many args":     ". {\n errors " + strings.Repeat("x ", 199000) + "\n}\n",
	} {
		start := time.Now()
		_, _ = PreviewCoreDNS(t.Context(), OfflineRequest{}, strings.NewReader(input))
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("%s took %v", name, d)
		}
	}
}

func rewriteNames(p *Plan) map[string]bool {
	out := map[string]bool{}
	for _, o := range p.Objects {
		for _, r := range o["spec"].(map[string]any)["rewrites"].([]any) {
			m := r.(map[string]any)
			typ, _ := m["type"].(string)
			out[m["name"].(string)+"/"+typ] = true
		}
	}
	return out
}

// CoreDNS sends each query to the server block with the longest matching
// zone, so local answers defined in a less specific block, or for reverse
// names no query reaches, were never served there and must not be imported.
func TestCoreDNSOfflineServerBlockRouting(t *testing.T) {
	p := corednsPreview(t, `. {
    hosts {
        192.168.1.10 nas.home.example nas.lan.example
        fallthrough
    }
    rewrite name tv.home.example media.lan.example
    rewrite name tv.lan.example media.lan.example
    forward . 192.0.2.53
    forward home.example 192.0.2.99
    cache
}
home.example {
    forward . 192.168.1.1
}
`)
	got := rewriteNames(p)
	for _, name := range []string{"nas.lan.example/", "10.1.168.192.in-addr.arpa/PTR", "tv.lan.example/"} {
		if !got[name] {
			t.Errorf("served answer %s missing: %v", name, got)
		}
	}
	if got["nas.home.example/"] || got["tv.home.example/"] {
		t.Fatalf("answers for names routed to the home.example block were imported: %v", got)
	}
	up := p.Settings["upstreams"].(map[string]any)
	if routes := up["conditional"].(map[string]string); len(routes) != 1 || len(up["groups"].(map[string]any)) != 2 {
		t.Fatalf("shadowed forward FROM was imported: %v %v", routes, up["groups"])
	}
	report := strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"Server block .:53: 2 hosts or rewrite names fall inside the more specific server block for home.example",
		"Server block .:53: forward FROM inside the more specific server block for home.example never matches; omitted.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("loss report lacks %q:\n%s", want, report)
		}
	}

	// Reverse names: a forward-zone block never receives PTR queries, and a
	// reverse block shadows the root block's PTR answers. Two blocks sharing
	// an address are valid and must not collide.
	p = corednsPreview(t, `. {
    hosts {
        10.0.0.5 five.example
        192.0.2.5 other.example
    }
}
10.0.0.0/8 {
    forward . 192.0.2.53
}
example.org {
    hosts {
        10.0.0.1 a.example.org
    }
}
example.net {
    hosts {
        10.0.0.1 b.example.net
    }
}
`)
	got = rewriteNames(p)
	for _, name := range []string{"five.example/", "other.example/", "5.2.0.192.in-addr.arpa/PTR", "a.example.org/", "b.example.net/"} {
		if !got[name] {
			t.Errorf("served answer %s missing: %v", name, got)
		}
	}
	if got["5.0.0.10.in-addr.arpa/PTR"] || got["1.0.0.10.in-addr.arpa/PTR"] {
		t.Fatalf("PTR answers CoreDNS never served were invented: %v", got)
	}
	report = strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"Server block .:53: 1 hosts PTR answers fall inside the more specific server block for 10.in-addr.arpa",
		"Server block example.org:53: 1 hosts PTR answers omitted; CoreDNS sends those queries to no server block or to another one.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("loss report lacks %q:\n%s", want, report)
		}
	}

	// A block mixing an encrypted key with a plain key keeps the plain key,
	// and each encrypted key is named per block.
	p = corednsPreview(t, "tls://example.org example.net {\n forward . 192.0.2.1\n}\ngrpc://a.example {\n}\ngrpc://b.example {\n}\n")
	if routes := p.Settings["upstreams"].(map[string]any)["conditional"].(map[string]string); len(routes) != 1 || routes["example.net."] == "" {
		t.Fatalf("plain key of a mixed block was not converted: %v", routes)
	}
	report = strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"Server block tls://example.org:853 example.net:53: encrypted or gRPC keys tls://example.org:853 are omitted with their certificates; the plain DNS keys are converted.",
		"Server block grpc://a.example:443: encrypted or gRPC listener block omitted with its certificates and directives;",
		"Server block grpc://b.example:443: encrypted or gRPC listener block omitted",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("loss report lacks %q:\n%s", want, report)
		}
	}
}

// Cache numbers outside the DNS TTL range or the capacity bound, or values
// kudurru reads differently (0 TTL is "no limit", 0 capacity turns the cache
// off), omit the cache directive as a unit.
func TestCoreDNSOfflineCacheBounds(t *testing.T) {
	for _, body := range []string{
		"cache 99999999999",
		"cache 2147483648",
		"cache {\n  success 100 0\n }",
		"cache {\n  denial 100 2147483648\n }",
		"cache {\n  denial 9223372036854775807\n }",
		"cache {\n  success 0\n }",
		"cache {\n  success 100 60 2147483648\n }",
		"cache {\n  success 100 x\n }",
	} {
		p := corednsPreview(t, ". {\n "+body+"\n}\n")
		if _, ok := p.Settings["cache"]; ok {
			t.Errorf("%q produced cache settings: %v", body, p.Settings["cache"])
		}
		if !strings.Contains(strings.Join(p.Warnings, "\n"), "cache with an invalid or out-of-range value omitted as a unit.") {
			t.Errorf("%q: loss report lacks the cache note: %v", body, p.Warnings)
		}
	}
	p := corednsPreview(t, ". {\n cache 2147483647 {\n  success 1073741824 60 0\n  denial 1 2147483647\n }\n}\n")
	cache := p.Settings["cache"].(map[string]any)
	if cache["maxEntries"] != 1073741825 || cache["maxTTL"] != "1m0s" || cache["negativeTTL"] != (2147483647*time.Second).String() || cache["minTTL"] != "0s" {
		t.Fatalf("in-range cache values: %v", cache)
	}
}
