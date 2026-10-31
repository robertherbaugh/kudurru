package dnsdist

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

// A transient lameduck SERVFAIL must not outlive backend removal in the
// resolver's temporary-failure cache and fault an otherwise healthy VIP.
func TestHealthQueriesBypassPacketCacheBeforePoolSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		zones []string
		cache bool
	}{
		{name: "resolver-cache"},
		{name: "authoritative-cache", zones: []string{"lab.invalid."}, cache: true},
		{name: "root-authoritative-cache", zones: []string{"."}, cache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			top, err := edge.Build(edge.Options{ProxyProtocolSecret: goldenSecret,
				AuthoritativeCache: tc.cache, AuthoritativeZones: tc.zones},
				[]edge.Backend{{Name: "a", Address: "192.0.2.1:5353"}, {Name: "b", Address: "192.0.2.2:5353"}}, goldenCreds)
			if err != nil {
				t.Fatal(err)
			}
			for _, qname := range []string{edge.HealthQName, "custom-health.lab.invalid."} {
				t.Run(qname, func(t *testing.T) {
					top.HealthCheck.QName = qname
					yfiles, err := (YAMLRenderer{}).Render(top)
					if err != nil {
						t.Fatal(err)
					}
					var cfg struct {
						Rules []struct {
							Selector map[string]any `yaml:"selector"`
							Action   map[string]any `yaml:"action"`
						} `yaml:"query_rules"`
					}
					if err := yaml.Unmarshal(yfiles[0].Data, &cfg); err != nil {
						t.Fatal(err)
					}
					bypass := -1
					for i, rule := range cfg.Rules {
						if rule.Action["type"] == "SetSkipCache" {
							if bypass != -1 || len(rule.Selector) != 2 || rule.Selector["type"] != "QName" || rule.Selector["qname"] != qname {
								t.Fatalf("health bypass must match only the exact configured name: %+v", rule.Selector)
							}
							bypass = i
						}
						if rule.Action["type"] == "Pool" && (bypass == -1 || bypass >= i) {
							t.Fatal("pool selection can cache a health response")
						}
					}
					if bypass <= 1 || cfg.Rules[0].Action["type"] != "RCode" || cfg.Rules[1].Action["type"] != "SetAdditionalProxyProtocolValue" {
						t.Fatal("health bypass must preserve the earlier transfer refusal and authentication TLV")
					}
					lfiles, err := (LuaRenderer{}).Render(top)
					if err != nil {
						t.Fatal(err)
					}
					lua := string(lfiles[0].Data)
					bypassAt := strings.Index(lua, "addAction(QNameRule("+luaString(qname)+"), SetSkipCacheAction()")
					if bypassAt < 0 || strings.Count(lua, "SetSkipCacheAction()") != 1 ||
						strings.Index(lua, "SetAdditionalProxyProtocolValueAction(") >= bypassAt ||
						strings.Index(lua, "PoolAction(") <= bypassAt {
						t.Fatal("Lua must bypass exact-name health caching after authentication and before every pool")
					}
				})
			}
		})
	}
}
