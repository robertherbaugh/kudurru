package importers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

var updateCoreDNS = flag.Bool("update-coredns", false, "rewrite testdata/coredns.golden.json")

func corednsPreview(t *testing.T, input string) *Plan {
	t.Helper()
	p, err := PreviewCoreDNS(t.Context(), OfflineRequest{Namespace: "dns"}, strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	// Every plan a test converts must be admissible: objects pass the CRD
	// validator kudurru apply and the import use, and the settings load and
	// validate over the defaults.
	validatePlan(t, p)
	validateCoreDNSSettings(t, p)
	return p
}

// validateCoreDNSSettings loads the plan's settings over the defaults, and
// over a destination with the strictest cache.minTTL the schema presets, with
// the configuration validation every node runs.
func validateCoreDNSSettings(t *testing.T, p *Plan) {
	t.Helper()
	for _, base := range []map[string]any{{}, {"cache": map[string]any{"minTTL": "1h"}}} {
		doc := map[string]any{}
		for k, v := range base {
			doc[k] = v
		}
		for k, v := range p.Settings {
			if cur, ok := doc[k].(map[string]any); ok {
				merged := map[string]any{}
				for kk, vv := range cur {
					merged[kk] = vv
				}
				for kk, vv := range v.(map[string]any) {
					merged[kk] = vv
				}
				doc[k] = merged
				continue
			}
			doc[k] = v
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "kudurru.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(path, nil); err != nil {
			t.Errorf("imported settings fail validation over %v: %v\n%s", base, err, raw)
		}
	}
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

// In-range cache values whose CoreDNS minimum TTL exceeds the maximum still
// convert to settings that validate: CoreDNS lets TTL win, so cache.minTTL
// is lowered to cache.maxTTL with a loss line. cache.minTTL is always
// emitted, so a destination minTTL cannot exceed the imported maxTTL.
func TestCoreDNSOfflineCacheMinTTLNeverExceedsMaxTTL(t *testing.T) {
	for body, want := range map[string][2]string{
		"cache 3":                             {"3s", "3s"},
		"cache {\n  success 100 10 3600\n }":  {"10s", "10s"},
		"cache 30":                            {"5s", "30s"},
		"cache {\n  success 100 600 3600\n }": {"10m0s", "10m0s"},
		"cache {\n  success 100 3600 60\n }":  {"1m0s", "1h0m0s"},
	} {
		p := corednsPreview(t, ". {\n "+body+"\n}\n")
		cache, _ := p.Settings["cache"].(map[string]any)
		if cache["minTTL"] != want[0] || cache["maxTTL"] != want[1] {
			t.Errorf("%q: cache = %v, want minTTL %s maxTTL %s", body, cache, want[0], want[1])
		}
		lowered := strings.Contains(strings.Join(p.Warnings, "\n"), "cache MINTTL above the success TTL becomes cache.minTTL equal to cache.maxTTL")
		if lowered != (want[0] == want[1]) {
			t.Errorf("%q: lowered note = %v: %v", body, lowered, p.Warnings)
		}
	}
}

// prefetch and serve_stale values are bounded like the other cache numbers,
// and serve_stale 0 says what kudurru does with it.
func TestCoreDNSOfflineCachePrefetchAndServeStaleBounds(t *testing.T) {
	for _, body := range []string{
		"cache {\n  prefetch 9223372036854775807 2562047h\n }",
		"cache {\n  prefetch 1073741825\n }",
		"cache {\n  prefetch 10 2562047h\n }",
	} {
		p := corednsPreview(t, ". {\n "+body+"\n}\n")
		if _, ok := p.Settings["cache"].(map[string]any)["prefetch"]; ok {
			t.Errorf("%q: unbounded prefetch emitted: %v", body, p.Settings["cache"])
		}
		if !strings.Contains(strings.Join(p.Warnings, "\n"), "cache prefetch with invalid or out-of-range values omitted.") {
			t.Errorf("%q: loss report lacks the prefetch line: %v", body, p.Warnings)
		}
	}
	p := corednsPreview(t, ". {\n cache {\n  serve_stale 2562047h\n }\n}\n")
	if _, ok := p.Settings["cache"].(map[string]any)["serveStale"]; ok || !strings.Contains(strings.Join(p.Warnings, "\n"), "cache serve_stale with invalid or out-of-range values omitted.") {
		t.Errorf("unbounded serve_stale: %v %v", p.Settings["cache"], p.Warnings)
	}
	p = corednsPreview(t, ". {\n cache {\n  prefetch 1073741824 596523h\n  serve_stale 0s\n }\n}\n")
	cache := p.Settings["cache"].(map[string]any)
	if pf := cache["prefetch"].(map[string]any); pf["threshold"] != 1073741824 || pf["window"] != "596523h0m0s" {
		t.Errorf("in-range prefetch: %v", pf)
	}
	if cache["serveStale"].(map[string]any)["ttl"] != "0s" || !strings.Contains(strings.Join(p.Warnings, "\n"), "cache serve_stale 0 becomes cache.serveStale.ttl 0s, which turns serving expired answers off.") {
		t.Errorf("serve_stale 0: %v %v", cache, p.Warnings)
	}
}

// Blocks the importer does not convert still take part in routing: a
// classless reverse prefix routes the zones CoreDNS expands it to, and a key
// that cannot be read at all leaves the routing unknown, so no local answer
// is imported as if it were served.
func TestCoreDNSOfflineUnreadableBlocksTakePartInRouting(t *testing.T) {
	p := corednsPreview(t, `. {
    hosts {
        10.1.1.10 nas.lan
        10.16.0.1 gw.lan
    }
    forward . 192.0.2.53
}
10.0.0.0/12 {
    forward . 192.168.1.1
}
`)
	got := rewriteNames(p)
	if got["10.1.1.10.in-addr.arpa/PTR"] {
		t.Fatalf("PTR answer shadowed by the classless block was imported: %v", got)
	}
	for _, name := range []string{"nas.lan/", "gw.lan/", "1.0.16.10.in-addr.arpa/PTR"} {
		if !got[name] {
			t.Errorf("served answer %s missing: %v", name, got)
		}
	}
	report := strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"Server block: classless reverse prefix key; block omitted, and local answers of other blocks for the reverse zones it covers are not imported (value redacted).",
		"Server block .:53: 1 hosts PTR answers fall inside the more specific server block for 1.10.in-addr.arpa",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("loss report lacks %q:\n%s", want, report)
		}
	}
	// IPv6 classless keys expand on nibble boundaries.
	if _, zones, ok := classlessKey("2001:db8::/30"); !ok || len(zones) != 4 || zones[0].name != "8.b.d.0.1.0.0.2.ip6.arpa." || zones[3].name != "b.b.d.0.1.0.0.2.ip6.arpa." {
		t.Fatalf("IPv6 classless expansion: %v %v", zones, ok)
	}

	for _, key := range []string{"{$LAB_ZONE}", "example.org:99999"} {
		p = corednsPreview(t, ". {\n hosts {\n  192.0.2.10 nas.home.example\n }\n rewrite name tv.home.example media.home.example\n forward . 192.0.2.53\n}\n"+key+" {\n forward . 192.168.1.1\n}\n")
		if len(rewriteNames(p)) != 0 {
			t.Fatalf("%s: local answers imported while a block with unreadable keys may take them: %v", key, rewriteNames(p))
		}
		report = strings.Join(p.Warnings, "\n")
		for _, want := range []string{
			"Server block .:53: 2 hosts or rewrite names omitted; a server block whose keys cannot be read may receive those queries in CoreDNS (names redacted).",
			"Server block .:53: 1 hosts PTR answers omitted; a server block whose keys cannot be read may receive those queries in CoreDNS (names redacted).",
		} {
			if !strings.Contains(report, want) {
				t.Errorf("%s: loss report lacks %q:\n%s", key, want, report)
			}
		}
		if strings.Contains(report, "LAB_ZONE") || strings.Contains(report, "99999") {
			t.Fatalf("unreadable key echoed: %s", report)
		}
	}
}

// CoreDNS routes per port: a block on 5353 does not shadow names of the
// root block on 53. The names are imported, and the loss line says that
// kudurru answers them alike on every listener.
func TestCoreDNSOfflinePortSpecificBlocksDoNotShadow(t *testing.T) {
	p := corednsPreview(t, `. {
    hosts {
        192.168.1.10 nas.home.example
        no_reverse
    }
    forward . 192.0.2.53
}
home.example:5353 {
    forward . 192.168.1.1
}
`)
	if got := rewriteNames(p); !got["nas.home.example/"] {
		t.Fatalf("name served on port 53 was omitted: %v", got)
	}
	report := strings.Join(p.Warnings, "\n")
	if strings.Contains(report, "fall inside the more specific server block") {
		t.Fatalf("port 5353 block reported as shadowing port 53:\n%s", report)
	}
	want := "Server block .:53: 1 hosts or rewrite names are imported, but on port 5353 CoreDNS sends those queries to another server block; kudurru gives them this block's answer on every listener (names redacted)."
	if !strings.Contains(report, want) {
		t.Errorf("loss report lacks %q:\n%s", want, report)
	}
	// On the same port the more specific block still shadows.
	p = corednsPreview(t, ".:5353 {\n hosts {\n  192.168.1.10 nas.home.example\n  no_reverse\n }\n}\nhome.example:5353 {\n forward . 192.168.1.1\n}\n")
	if got := rewriteNames(p); got["nas.home.example/"] {
		t.Fatalf("shadowed name on the same port was imported: %v", got)
	}
}
