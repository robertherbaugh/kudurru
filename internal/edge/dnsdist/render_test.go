package dnsdist

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/edge"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// goldenCreds are fixed so the golden files are stable; production
// credentials are random per start (edge.NewCredentials).
var goldenCreds = edge.Credentials{
	ConsoleKey: "a3VkdXJydS1nb2xkZW4tY29uc29sZS1rZXktMzJieXQ=",
	APIKey:     "golden-api-key-0123456789abcdef",
	Password:   "golden-dashboard-password",
}

const goldenSecret = "6b75647572752d70726f78792d7365637265742d676f6c64656e2d3332627974"

func backends(eps []cluster.Endpoint) []edge.Backend {
	out := make([]edge.Backend, 0, len(eps))
	for _, e := range eps {
		out = append(out, edge.Backend{Name: e.Name, Address: e.Address, InternalAddress: e.InternalAddress})
	}
	return out
}

func endpointSliceBackends(t *testing.T) []edge.Backend {
	t.Helper()
	yes, no := ptr.To(true), ptr.To(false)
	ep := func(name, ip string, ready bool) discoveryv1.Endpoint {
		r := ptr.To(ready)
		return discoveryv1.Endpoint{Addresses: []string{ip}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: name},
			Conditions: discoveryv1.EndpointConditions{Ready: r, Serving: yes, Terminating: no}}
	}
	cs := fake.NewClientset(&discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "kudurru-dns-x7k2p", Namespace: "kudurru", Labels: map[string]string{discoveryv1.LabelServiceName: "kudurru-dns"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("dns"), Port: ptr.To[int32](5353)}},
		Endpoints: []discoveryv1.Endpoint{
			ep("kudurru-6d9f7c5b8-2xq4m", "10.244.1.17", true),
			ep("kudurru-6d9f7c5b8-9kz8w", "10.244.2.23", true),
			ep("kudurru-6d9f7c5b8-tn5vh", "10.244.3.9", false), // not ready: filtered
		},
	})
	src := &cluster.EndpointSliceSource{Client: cs.DiscoveryV1(), Namespace: "kudurru", Service: "kudurru-dns"}
	eps, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return backends(cluster.FilterServing(eps))
}

func goldenCases(t *testing.T) map[string]*edge.Topology {
	t.Helper()
	build := func(o edge.Options, b []edge.Backend) *edge.Topology {
		top, err := edge.Build(o, b, goldenCreds)
		if err != nil {
			t.Fatal(err)
		}
		return top
	}
	static3, err := cluster.ParseStaticEndpoints("10.0.0.11:5353,10.0.0.12:5353,10.0.0.13:5353", 5353, 8082)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*edge.Topology{
		// compose `edge` profile: one kudurru on loopback, no secret (D56).
		"single-loopback": build(edge.Options{ServerID: "edge"},
			[]edge.Backend{{Name: "kudurru", Address: "127.0.0.1:5353"}}),
		// compose ha-static: every node lists all three kudurru nodes.
		"static-3": build(edge.Options{ServerID: "node-a", ProxyProtocolSecret: goldenSecret,
			TransferSources: []string{"10.0.0.0/24"}}, backends(static3)),
		// Kubernetes: kudurru-edge Deployment fed by the headless EndpointSlice.
		"endpointslice": build(edge.Options{ServerID: "kudurru-edge-5f8d7-abcde", ProxyProtocolSecret: goldenSecret,
			ACL: []string{"0.0.0.0/0", "::/0"}}, endpointSliceBackends(t)),
		// Every frontend protocol from a cert-manager Secret.
		"tls": build(edge.Options{ServerID: "node-a", ProxyProtocolSecret: goldenSecret,
			CertFile: "/certs/tls.crt", KeyFile: "/certs/tls.key", Listen: []string{"0.0.0.0", "::"}},
			[]edge.Backend{{Name: "node-a", Address: "10.0.0.11:5353"}, {Name: "node-b", Address: "10.0.0.12:5353"}}),
		// edge.authoritativeCache: short cache, zones routed, flushed on serial change.
		"authoritative-cache": build(edge.Options{ServerID: "node-a", ProxyProtocolSecret: goldenSecret,
			AuthoritativeCache: true, AuthoritativeCacheTTL: 20,
			AuthoritativeZones: []string{"Example.com", "10.in-addr.arpa.", "example.com.", "."},
			TransferSources:    []string{"192.0.2.53", "2001:db8::/64"}},
			[]edge.Backend{{Name: "node-a", Address: "10.0.0.11:5353"}, {Name: "node-b", Address: "10.0.0.12:5353"}}),
		// Opt-ins: DynBlocks and the eBPF filter.
		"dynblocks-ebpf": build(edge.Options{ServerID: "node-a", ProxyProtocolSecret: goldenSecret, Threads: 4,
			DynBlocks: edge.DynBlocks{Enabled: true, QueryRate: 50, NXDomainRate: 20, Exclude: []string{"10.0.0.0/8"}},
			EBPF:      edge.EBPF{Enabled: true}},
			[]edge.Backend{{Address: "10.0.0.11:5353"}}),
	}
}

func TestRenderGolden(t *testing.T) {
	for name, top := range goldenCases(t) {
		for _, r := range []Renderer{YAMLRenderer{}, LuaRenderer{}} {
			t.Run(name+"/"+r.Name(), func(t *testing.T) {
				files, err := r.Render(top)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range files {
					golden := filepath.Join("testdata", name, f.Name)
					checkGolden(t, golden, f.Data)
					switch filepath.Ext(f.Name) {
					case ".yml":
						checkYAML(t, f.Data, top)
					default:
						checkLua(t, f.Name, f.Data)
					}
				}
				again, _ := r.Render(top)
				for i := range files {
					if !bytes.Equal(files[i].Data, again[i].Data) {
						t.Errorf("%s: rendering is not deterministic", files[i].Name)
					}
				}
			})
		}
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run go test ./internal/edge/dnsdist -update and review the diff)\n--- got ---\n%s", path, got)
	}
}

// checkYAML parses the rendered file back and asserts the §3.6 invariants.
func checkYAML(t *testing.T, data []byte, top *edge.Topology) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("rendered YAML does not parse: %v", err)
	}
	for _, k := range []string{"acl", "console", "webserver", "binds", "pools", "backends", "query_rules", "load_balancing_policies"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	if bytes.Contains(data, []byte("enableLuaConfiguration")) {
		t.Error("enableLuaConfiguration must never be rendered")
	}
	bes, _ := doc["backends"].([]any)
	if len(bes) != len(top.Backends) {
		t.Fatalf("%d backends rendered, want %d", len(bes), len(top.Backends))
	}
	for _, b := range bes {
		m := b.(map[string]any)
		if m["use_proxy_protocol"] != true {
			t.Errorf("backend %v without use_proxy_protocol", m["name"])
		}
		hc := m["health_checks"].(map[string]any)
		if hc["qname"] != edge.HealthQName || hc["qtype"] != "TXT" || hc["must_resolve"] != true || hc["interval"] != 1 || hc["max_failures"] != 2 || hc["rise"] != 2 {
			t.Errorf("backend %v health check %v", m["name"], hc)
		}
	}
	rules, _ := doc["query_rules"].([]any)
	first := rules[0].(map[string]any)
	if first["name"] != ruleRefuseTransfers {
		t.Errorf("first rule is %v, want the transfer refusal", first["name"])
	}
	hasTLV := bytes.Contains(data, []byte("proxy_type: 224"))
	if hasTLV != (top.ProxyProtocolSecret != "") {
		t.Errorf("TLV 0xE0 rendered = %v, secret set = %v", hasTLV, top.ProxyProtocolSecret != "")
	}
	ws := doc["webserver"].(map[string]any)
	if ws["hash_plaintext_credentials"] != true || ws["api_read_write"] != false {
		t.Errorf("webserver %v", ws)
	}
}

// checkLua is a syntactic sanity check: balanced brackets and quotes and
// block keywords matched by `end`. The real parser runs in `make
// edge-check` (dnsdist --check-config inside the pinned image).
func checkLua(t *testing.T, name string, data []byte) {
	t.Helper()
	src := string(data)
	if strings.Contains(src, "enableLuaConfiguration") {
		t.Errorf("%s: enableLuaConfiguration must never be rendered", name)
	}
	var stack []byte
	opens, ends := 0, 0
	inStr := byte(0)
	word := strings.Builder{}
	flush := func() {
		switch word.String() {
		case "function", "do", "then":
			if word.String() != "then" {
				opens++
			}
		case "if":
			opens++
		case "end":
			ends++
		}
		word.Reset()
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			if inStr != 0 {
				if c == '\\' {
					i++
				} else if c == inStr {
					inStr = 0
				}
				continue
			}
			if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				word.WriteByte(c)
				continue
			}
			flush()
			switch c {
			case '"', '\'':
				inStr = c
			case '(', '{', '[':
				stack = append(stack, c)
			case ')', '}', ']':
				want := map[byte]byte{')': '(', '}': '{', ']': '['}[c]
				if len(stack) == 0 || stack[len(stack)-1] != want {
					t.Fatalf("%s: unbalanced %q in line %q", name, c, line)
				}
				stack = stack[:len(stack)-1]
			}
		}
		flush()
		if inStr != 0 {
			t.Fatalf("%s: unterminated string in line %q", name, line)
		}
	}
	if len(stack) != 0 {
		t.Errorf("%s: %d unclosed brackets", name, len(stack))
	}
	// "for ... do" counts once through "do".
	if opens != ends {
		t.Errorf("%s: %d blocks opened, %d closed", name, opens, ends)
	}
}

func TestRenderRejectsInvalidTopology(t *testing.T) {
	good, err := edge.Build(edge.Options{ServerID: "e"}, []edge.Backend{{Name: "n", Address: "127.0.0.1:5353"}}, goldenCreds)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*edge.Topology){
		"public console":    func(t *edge.Topology) { t.Console.Address = "0.0.0.0:5199" },
		"public api":        func(t *edge.Topology) { t.Webserver.Address = "10.0.0.1:8083" },
		"short secret":      func(t *edge.Topology) { t.ProxyProtocolSecret = "short" },
		"binary secret":     func(t *edge.Topology) { t.ProxyProtocolSecret = strings.Repeat("\x01", 40) },
		"tls without cert":  func(t *edge.Topology) { t.Binds = append(t.Binds, edge.Bind{Address: "0.0.0.0:853", Protocol: edge.DoT}) },
		"bad policy":        func(t *edge.Topology) { t.Pools[0].Policy = "orderedWhashed" },
		"quote in name":     func(t *edge.Topology) { t.Backends[0].Name = `x") os.exit(1) --` },
		"bad console key":   func(t *edge.Topology) { t.Console.Key = "not-base64" },
		"duplicate backend": func(t *edge.Topology) { t.Backends = append(t.Backends, t.Backends[0]) },
		"bad acl":           func(t *edge.Topology) { t.ACL = []string{"10.0.0.1"} },
	}
	for name, mut := range mutations {
		c := *good
		c.Backends = append([]edge.Backend(nil), good.Backends...)
		c.Binds = append([]edge.Bind(nil), good.Binds...)
		c.Pools = append([]edge.Pool(nil), good.Pools...)
		mut(&c)
		for _, r := range []Renderer{YAMLRenderer{}, LuaRenderer{}} {
			if _, err := r.Render(&c); err == nil {
				t.Errorf("%s/%s: rendered an invalid topology", name, r.Name())
			}
		}
	}
}

func TestBackendIDStable(t *testing.T) {
	// Pinned: changing the namespace or the derivation reshuffles every
	// chashed ring in production.
	if got := edge.BackendID("node-a"); got != edge.BackendID("node-a") || len(got) != 36 || got[14] != '5' {
		t.Fatalf("BackendID(node-a) = %s, want a stable version-5 uuid", got)
	}
	if edge.BackendID("node-a") == edge.BackendID("node-b") {
		t.Fatal("distinct nodes share an id")
	}
}

func TestNewRenderer(t *testing.T) {
	for in, want := range map[string]string{"": "yaml", "yaml": "yaml", "YML": "yaml", "lua": "lua"} {
		r, err := NewRenderer(in)
		if err != nil || r.Name() != want {
			t.Errorf("%q: %v %v", in, r, err)
		}
	}
	if _, err := NewRenderer("toml"); err == nil {
		t.Error("toml accepted")
	}
}
