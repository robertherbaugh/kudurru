package dnsdist

import (
	"bytes"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

// YAMLRenderer renders dnsdist 2.1 `dnsdist.yml` plus the sibling
// `dnsdist.lua` holding the health-check validation functions (functions
// only: dnsdist loads the sibling without configuration directives, and
// kudurru never calls enableLuaConfiguration()). Field names follow
// dnsdist-settings-definitions.yml of the 2.1 branch; `make edge-check`
// runs `dnsdist --check-config` on the golden files inside the pinned
// image.
type YAMLRenderer struct{}

// Name implements Renderer.
func (YAMLRenderer) Name() string { return "yaml" }

// Main implements Renderer.
func (YAMLRenderer) Main() string { return "dnsdist.yml" }

// Render implements Renderer.
func (r YAMLRenderer) Render(in *edge.Topology) ([]File, error) {
	t, err := prepare(in)
	if err != nil {
		return nil, err
	}
	cfg := buildYAML(t)
	var buf bytes.Buffer
	buf.WriteString("# " + header + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("dnsdist: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	lua := "-- " + header + "\n-- Functions only: loaded next to dnsdist.yml without configuration directives.\n"
	if v := luaValidators(t); v != "" {
		lua += "\n" + v
	}
	return []File{{Name: r.Main(), Data: buf.Bytes()}, {Name: "dnsdist.lua", Data: []byte(lua)}}, nil
}

type yConfig struct {
	ACL                   []string        `yaml:"acl"`
	Console               yConsole        `yaml:"console"`
	Webserver             yWebserver      `yaml:"webserver"`
	General               yGeneral        `yaml:"general"`
	LoadBalancingPolicies yLB             `yaml:"load_balancing_policies"`
	Binds                 []yBind         `yaml:"binds"`
	PacketCaches          []yPacketCache  `yaml:"packet_caches,omitempty"`
	Pools                 []yPool         `yaml:"pools"`
	Backends              []yBackend      `yaml:"backends"`
	QueryRules            []yRule         `yaml:"query_rules"`
	DynamicRules          []yDynamicGroup `yaml:"dynamic_rules,omitempty"`
	EBPF                  *yEBPF          `yaml:"ebpf,omitempty"`
}

type yConsole struct {
	ListenAddress string   `yaml:"listen_address"`
	Key           string   `yaml:"key"`
	ACL           []string `yaml:"acl"`
}

type yWebserver struct {
	ListenAddresses                 []string `yaml:"listen_addresses"`
	APIKey                          string   `yaml:"api_key"`
	Password                        string   `yaml:"password"`
	HashPlaintextCredentials        bool     `yaml:"hash_plaintext_credentials"`
	ACL                             []string `yaml:"acl"`
	APIRequiresAuthentication       bool     `yaml:"api_requires_authentication"`
	StatsRequireAuthentication      bool     `yaml:"stats_require_authentication"`
	DashboardRequiresAuthentication bool     `yaml:"dashboard_requires_authentication"`
	APIReadWrite                    bool     `yaml:"api_read_write"`
}

type yGeneral struct {
	ServerID             string   `yaml:"server_id,omitempty"`
	CapabilitiesToRetain []string `yaml:"capabilities_to_retain,omitempty"`
}

type yLB struct {
	DefaultPolicy      string `yaml:"default_policy"`
	ServfailOnNoServer bool   `yaml:"servfail_on_no_server"`
}

type yBind struct {
	ListenAddress string `yaml:"listen_address"`
	Protocol      string `yaml:"protocol"`
	ReusePort     bool   `yaml:"reuseport,omitempty"`
	Threads       int    `yaml:"threads,omitempty"`
	TLS           *yTLS  `yaml:"tls,omitempty"`
	DoH           *yDoH  `yaml:"doh,omitempty"`
}

type yTLS struct {
	Certificates   []yCert `yaml:"certificates"`
	MinimumVersion string  `yaml:"minimum_version"`
}

type yCert struct {
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
}

type yDoH struct {
	Paths []string `yaml:"paths"`
}

type yPacketCache struct {
	Name                string `yaml:"name"`
	Size                int    `yaml:"size"`
	MaxTTL              int    `yaml:"max_ttl"`
	MinTTL              int    `yaml:"min_ttl"`
	StaleTTL            int    `yaml:"stale_ttl"`
	TemporaryFailureTTL int    `yaml:"temporary_failure_ttl"`
	MaxNegativeTTL      int    `yaml:"max_negative_ttl"`
}

type yPool struct {
	Name        string `yaml:"name"`
	Policy      string `yaml:"policy"`
	PacketCache string `yaml:"packet_cache,omitempty"`
}

type yBackend struct {
	Address          string        `yaml:"address"`
	ID               string        `yaml:"id"`
	Name             string        `yaml:"name"`
	Protocol         string        `yaml:"protocol"`
	Pools            []string      `yaml:"pools"`
	UseProxyProtocol bool          `yaml:"use_proxy_protocol"`
	Weight           int           `yaml:"weight"`
	Order            int           `yaml:"order"`
	Sockets          int           `yaml:"sockets"`
	HealthChecks     yHealthChecks `yaml:"health_checks"`
}

type yHealthChecks struct {
	Mode               string `yaml:"mode"`
	QName              string `yaml:"qname"`
	QType              string `yaml:"qtype"`
	Interval           int    `yaml:"interval"`
	Timeout            int    `yaml:"timeout"`
	MaxFailures        int    `yaml:"max_failures"`
	Rise               int    `yaml:"rise"`
	MustResolve        bool   `yaml:"must_resolve"`
	ValidationFunction string `yaml:"validation_function,omitempty"`
}

type yRule struct {
	Name     string    `yaml:"name"`
	UUID     string    `yaml:"uuid"`
	Selector ySelector `yaml:"selector"`
	Action   yAction   `yaml:"action"`
}

type ySelector struct {
	Type      string      `yaml:"type"`
	QName     string      `yaml:"qname,omitempty"`
	Code      string      `yaml:"code,omitempty"`
	QType     string      `yaml:"qtype,omitempty"`
	Suffixes  []string    `yaml:"suffixes,omitempty"`
	Netmasks  []string    `yaml:"netmasks,omitempty"`
	Selectors []ySelector `yaml:"selectors,omitempty"`
	Selector  *ySelector  `yaml:"selector,omitempty"`
}

type yAction struct {
	Type      string `yaml:"type"`
	RCode     string `yaml:"rcode,omitempty"`
	PoolName  string `yaml:"pool_name,omitempty"`
	ProxyType *int   `yaml:"proxy_type,omitempty"`
	Value     string `yaml:"value,omitempty"`
}

type yDynamicGroup struct {
	Name          string         `yaml:"name"`
	MaskIPv4      int            `yaml:"mask_ipv4"`
	MaskIPv6      int            `yaml:"mask_ipv6"`
	ExcludeRanges []string       `yaml:"exclude_ranges,omitempty"`
	Rules         []yDynamicRule `yaml:"rules"`
}

type yDynamicRule struct {
	Type           string `yaml:"type"`
	RCode          string `yaml:"rcode,omitempty"`
	Rate           int    `yaml:"rate"`
	Seconds        int    `yaml:"seconds"`
	ActionDuration int    `yaml:"action_duration"`
	Comment        string `yaml:"comment"`
	Action         string `yaml:"action"`
}

type yEBPF struct {
	IPv4   yEBPFMap `yaml:"ipv4"`
	IPv6   yEBPFMap `yaml:"ipv6"`
	QNames yEBPFMap `yaml:"qnames"`
}

type yEBPFMap struct {
	MaxEntries int `yaml:"max_entries"`
}

var loopbackACL = []string{"127.0.0.1/32", "::1/128"}

func buildYAML(t *edge.Topology) yConfig {
	cfg := yConfig{
		ACL:     t.ACL,
		Console: yConsole{ListenAddress: t.Console.Address, Key: t.Console.Key, ACL: loopbackACL},
		Webserver: yWebserver{
			ListenAddresses:                 []string{t.Webserver.Address},
			APIKey:                          t.Webserver.APIKey,
			Password:                        t.Webserver.Password,
			HashPlaintextCredentials:        true,
			ACL:                             loopbackACL,
			APIRequiresAuthentication:       true,
			StatsRequireAuthentication:      true,
			DashboardRequiresAuthentication: true,
		},
		General:               yGeneral{ServerID: t.ServerID},
		LoadBalancingPolicies: yLB{DefaultPolicy: "leastOutstanding", ServfailOnNoServer: true},
		Backends:              []yBackend{},
	}
	for _, b := range t.Binds {
		yb := yBind{ListenAddress: b.Address, Protocol: string(b.Protocol)}
		if b.Protocol == edge.Do53 {
			yb.ReusePort = b.ReusePort
			yb.Threads = b.Threads
		}
		if b.Protocol.TLS() {
			yb.TLS = &yTLS{Certificates: []yCert{{Certificate: b.CertFile, Key: b.KeyFile}}, MinimumVersion: "tls1.2"}
		}
		if b.Protocol == edge.DoH || b.Protocol == edge.DoH3 {
			yb.DoH = &yDoH{Paths: b.DoHPaths}
		}
		cfg.Binds = append(cfg.Binds, yb)
	}
	for _, p := range t.Pools {
		yp := yPool{Name: p.Name, Policy: p.Policy}
		if c := p.Cache; c != nil {
			yp.PacketCache = c.Name
			cfg.PacketCaches = append(cfg.PacketCaches, yPacketCache{
				Name: c.Name, Size: c.Size, MaxTTL: c.MaxTTL, MinTTL: c.MinTTL, StaleTTL: c.StaleTTL,
				TemporaryFailureTTL: c.TemporaryFailureTTL, MaxNegativeTTL: c.MaxNegativeTTL,
			})
		}
		cfg.Pools = append(cfg.Pools, yp)
	}
	hc := t.HealthCheck
	for _, b := range t.Backends {
		cfg.Backends = append(cfg.Backends, yBackend{
			Address: b.Address, ID: b.ID, Name: b.DisplayName(), Protocol: "Do53", Pools: b.Pools,
			UseProxyProtocol: b.ProxyProtocol, Weight: b.Weight, Order: b.Order, Sockets: b.Sockets,
			HealthChecks: yHealthChecks{
				Mode: "auto", QName: hc.QName, QType: hc.QType, Interval: hc.Interval, Timeout: hc.Timeout,
				MaxFailures: hc.MaxFailures, Rise: hc.Rise, MustResolve: hc.MustResolve,
				ValidationFunction: validatorName(t, b),
			},
		})
	}
	cfg.QueryRules = yamlRules(t)
	if d := t.DynBlocks; d.Enabled {
		g := yDynamicGroup{Name: "kudurru", MaskIPv4: 32, MaskIPv6: 64, ExcludeRanges: d.Exclude}
		if d.QueryRate > 0 {
			g.Rules = append(g.Rules, yDynamicRule{Type: "query-rate", Rate: d.QueryRate, Seconds: d.Window, ActionDuration: d.BlockFor, Comment: "kudurru: query rate exceeded", Action: d.Action})
		}
		if d.NXDomainRate > 0 {
			g.Rules = append(g.Rules, yDynamicRule{Type: "rcode-rate", RCode: "NXDomain", Rate: d.NXDomainRate, Seconds: d.Window, ActionDuration: d.BlockFor, Comment: "kudurru: NXDOMAIN rate exceeded", Action: d.Action})
		}
		cfg.DynamicRules = []yDynamicGroup{g}
	}
	if e := t.EBPF; e.Enabled {
		cfg.EBPF = &yEBPF{IPv4: yEBPFMap{e.IPv4MaxItems}, IPv6: yEBPFMap{e.IPv6MaxItems}, QNames: yEBPFMap{e.QNameMaxItems}}
		cfg.General.CapabilitiesToRetain = []string{"CAP_SYS_ADMIN"}
	}
	return cfg
}

// transferSelector matches AXFR, IXFR, NOTIFY and UPDATE.
func transferSelector() ySelector {
	return ySelector{Type: "Or", Selectors: []ySelector{
		{Type: "Opcode", Code: "Notify"},
		{Type: "Opcode", Code: "Update"},
		{Type: "QType", QType: "AXFR"},
		{Type: "QType", QType: "IXFR"},
	}}
}

func yamlRules(t *edge.Topology) []yRule {
	refuse := transferSelector()
	if len(t.Rules.TransferSources) > 0 {
		refuse = ySelector{Type: "And", Selectors: []ySelector{
			refuse,
			{Type: "Not", Selector: &ySelector{Type: "NetmaskGroup", Netmasks: t.Rules.TransferSources}},
		}}
	}
	rules := []yRule{{Name: ruleRefuseTransfers, UUID: ruleUUID(ruleRefuseTransfers), Selector: refuse, Action: yAction{Type: "RCode", RCode: "Refused"}}}
	if t.ProxyProtocolSecret != "" {
		pt := edge.ProxyTLVSecret
		rules = append(rules, yRule{
			Name: ruleProxySecret, UUID: ruleUUID(ruleProxySecret), Selector: ySelector{Type: "All"},
			Action: yAction{Type: "SetAdditionalProxyProtocolValue", ProxyType: &pt, Value: t.ProxyProtocolSecret},
		})
	}
	// Health can briefly SERVFAIL before active checks remove a draining
	// backend. Never retain that failure after a healthy peer becomes usable.
	rules = append(rules, yRule{
		Name: ruleHealthNoCache, UUID: ruleUUID(ruleHealthNoCache),
		Selector: ySelector{Type: "QName", QName: t.HealthCheck.QName},
		Action:   yAction{Type: "SetSkipCache"},
	})
	if len(t.Rules.AuthoritativeZones) > 0 {
		rules = append(rules, yRule{
			Name: ruleAuthoritative, UUID: ruleUUID(ruleAuthoritative),
			Selector: ySelector{Type: "QNameSuffix", Suffixes: t.Rules.AuthoritativeZones},
			Action:   yAction{Type: "Pool", PoolName: edge.PoolAuthoritative},
		})
	}
	rules = append(rules, yRule{
		Name: ruleResolver, UUID: ruleUUID(ruleResolver), Selector: ySelector{Type: "All"},
		Action: yAction{Type: "Pool", PoolName: edge.PoolResolver},
	})
	return rules
}
