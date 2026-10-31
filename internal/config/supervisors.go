package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Edge configures the existing host-local dnsdist render/run commands. Secrets
// remain file references; these settings never start another runtime.
type Edge struct {
	Backends                []string `koanf:"backends" json:"backends"`
	BackendsFrom            string   `koanf:"backendsFrom" json:"backendsFrom"`
	BackendPort             int      `koanf:"backendPort" json:"backendPort"`
	BackendPortName         string   `koanf:"backendPortName" json:"backendPortName"`
	InternalPort            int      `koanf:"internalPort" json:"internalPort"`
	PoolResolver            string   `koanf:"poolResolver" json:"poolResolver"`
	PoolAuthoritative       string   `koanf:"poolAuthoritative" json:"poolAuthoritative"`
	TLSDir                  string   `koanf:"tlsDir" json:"tlsDir"`
	Listen                  []string `koanf:"listen" json:"listen"`
	Protocols               []string `koanf:"protocols" json:"protocols"`
	DoT                     string   `koanf:"dot" json:"dot"`
	DoH                     string   `koanf:"doh" json:"doh"`
	DoQ                     string   `koanf:"doq" json:"doq"`
	DoH3                    string   `koanf:"doh3" json:"doh3"`
	API                     string   `koanf:"api" json:"api"`
	Console                 string   `koanf:"console" json:"console"`
	APIKeyOut               string   `koanf:"apiKeyOut" json:"apiKeyOut"`
	APIKeyGroup             string   `koanf:"apiKeyGroup" json:"apiKeyGroup"`
	ProxyProtocolSecretFile string   `koanf:"proxyProtocolSecretFile" json:"proxyProtocolSecretFile"`
	AuthoritativeCache      bool     `koanf:"authoritativeCache" json:"authoritativeCache"`
	AuthoritativeCacheTTL   int      `koanf:"authoritativeCacheTTL" json:"authoritativeCacheTTL"`
	AuthoritativeZones      []string `koanf:"authoritativeZones" json:"authoritativeZones"`
	TransferSources         []string `koanf:"transferSources" json:"transferSources"`
	ACL                     []string `koanf:"acl" json:"acl"`
	CacheSize               int      `koanf:"cacheSize" json:"cacheSize"`
	Dynblocks               bool     `koanf:"dynblocks" json:"dynblocks"`
	DynblocksQPS            int      `koanf:"dynblocksQPS" json:"dynblocksQPS"`
	DynblocksNXDomainRate   int      `koanf:"dynblocksNXDomainRate" json:"dynblocksNXDomainRate"`
	EBPF                    bool     `koanf:"ebpf" json:"ebpf"`
	Renderer                string   `koanf:"renderer" json:"renderer"`
	ServerID                string   `koanf:"serverID" json:"serverID"`
	Threads                 int      `koanf:"threads" json:"threads"`
	DNSDistBin              string   `koanf:"dnsdistBin" json:"dnsdistBin"`
	DNSDistUser             string   `koanf:"dnsdistUser" json:"dnsdistUser"`
	ConfigDir               string   `koanf:"configDir" json:"configDir"`
	HealthAddress           string   `koanf:"healthAddress" json:"healthAddress"`
	Debounce                Duration `koanf:"debounce" json:"debounce"`
	Drain                   Duration `koanf:"drain" json:"drain"`
	ZonePoll                Duration `koanf:"zonePoll" json:"zonePoll"`
	PollZones               bool     `koanf:"pollZones" json:"pollZones"`
	StopTimeout             Duration `koanf:"stopTimeout" json:"stopTimeout"`
	InternalTokenFile       string   `koanf:"internalTokenFile" json:"internalTokenFile"`
	InternalCAFile          string   `koanf:"internalCAFile" json:"internalCAFile"`
	InternalServerName      string   `koanf:"internalServerName" json:"internalServerName"`
	LogLevel                string   `koanf:"logLevel" json:"logLevel"`
	LogFormat               string   `koanf:"logFormat" json:"logFormat"`
}

// VIP configures the existing host-local keepalived render/run commands.
type VIP struct {
	Observation       VIPObservation `koanf:"observation" json:"observation"`
	VIPs              []VIPInstance  `koanf:"vips" json:"vips"`
	PreemptDelay      map[string]int `koanf:"preemptDelay" json:"preemptDelay"`
	Peers             []string       `koanf:"peers" json:"peers"`
	PeersFile         string         `koanf:"peersFile" json:"peersFile"`
	PeersFrom         string         `koanf:"peersFrom" json:"peersFrom"`
	PriorityIndex     int            `koanf:"priorityIndex" json:"priorityIndex"`
	NodeCount         int            `koanf:"nodeCount" json:"nodeCount"`
	UnicastSrc        string         `koanf:"unicastSrc" json:"unicastSrc"`
	VRRPInterface     string         `koanf:"vrrpInterface" json:"vrrpInterface"`
	HMACKeyFile       string         `koanf:"hmacKeyFile" json:"hmacKeyFile"`
	Legacy            bool           `koanf:"legacy" json:"legacy"`
	TrackDNSDistAPI   string         `koanf:"trackDNSDistAPI" json:"trackDNSDistAPI"`
	DNSDistAPIKeyFile string         `koanf:"dnsdistAPIKeyFile" json:"dnsdistAPIKeyFile"`
	RequireBackends   int            `koanf:"requireBackends" json:"requireBackends"`
	ProbeCommand      string         `koanf:"probeCommand" json:"probeCommand"`
	RunDir            string         `koanf:"runDir" json:"runDir"`
	RouterID          string         `koanf:"routerID" json:"routerID"`
	KudurruBin        string         `koanf:"kudurruBin" json:"kudurruBin"`
	KeepalivedBin     string         `koanf:"keepalivedBin" json:"keepalivedBin"`
	ConfigPath        string         `koanf:"configPath" json:"configPath"`
	HealthAddress     string         `koanf:"healthAddress" json:"healthAddress"`
	StopTimeout       Duration       `koanf:"stopTimeout" json:"stopTimeout"`
	LogLevel          string         `koanf:"logLevel" json:"logLevel"`
	LogFormat         string         `koanf:"logFormat" json:"logFormat"`
}

// VIPInstance is an operator-assigned address and immutable host VRID.
type VIPInstance struct {
	Name      string `koanf:"name" json:"name"`
	Address   string `koanf:"address" json:"address"`
	Interface string `koanf:"interface" json:"interface"`
	VRID      int    `koanf:"vrid" json:"vrid"`
}

var vipInstanceShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "name", Label: "Name", Help: "Unique VIP name used in keepalived instance and script identifiers.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "address", Label: "Address", Help: "VIP address with prefix length, for example 192.0.2.53/24.", Required: true, Shape: Shape{Type: TypeString, Format: FormatCIDR}},
	{Name: "interface", Label: "Interface", Help: "Host interface receiving the VIP address.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "vrid", Label: "VRID", Help: "Operator-assigned virtual router ID, unique on this host; never allocated by an API write.", Required: true, Shape: Shape{Type: TypeInt, Min: new(1), Max: new(255)}},
}}

var (
	edgeProtocols = []Option{
		{Value: "do53", Label: "DNS", Help: "Plain DNS over UDP and TCP."},
		{Value: "dot", Label: "DoT", Help: "DNS over TLS."},
		{Value: "doh", Label: "DoH", Help: "DNS over HTTPS."},
		{Value: "doq", Label: "DoQ", Help: "DNS over QUIC."},
		{Value: "doh3", Label: "DoH3", Help: "DNS over HTTP/3."},
	}
	supervisorRenderers = []Option{
		{Value: "yaml", Label: "YAML", Help: "dnsdist.yml plus functions-only dnsdist.lua."},
		{Value: "lua", Label: "Lua", Help: "Standalone dnsdist.conf."},
	}
	supervisorLogLevels = []Option{
		{Value: "debug", Label: "Debug", Help: "Detailed troubleshooting output."},
		{Value: "info", Label: "Info", Help: "Normal operations and changes."},
		{Value: "warn", Label: "Warning", Help: "Warnings and errors."},
		{Value: "error", Label: "Error", Help: "Errors only."},
	}
	supervisorLogFormats = []Option{
		{Value: "json", Label: "JSON", Help: "Structured JSON log lines."},
		{Value: "text", Label: "Text", Help: "Human-readable text log lines."},
	}
)

// SupervisorConfigured distinguishes an omitted priority from an explicit zero
// in a file, environment or flag before Node discovery supplies a default.
func (c *Config) SupervisorConfigured(path string) bool {
	return c.supervisorConfigured[path]
}

func supervisorKey(path string) bool {
	return strings.HasPrefix(path, "edge.") || strings.HasPrefix(path, "vip.") && !strings.HasPrefix(path, "vip.observation.")
}

// Only the new structured supervisor keys accept JSON environment values.
// Other structured settings retain the existing config-file-only rule.
func coerceSupervisor(key Key, value string) (any, error) {
	switch key.Path {
	case "vip.vips":
		var typed []VIPInstance
		if err := json.Unmarshal([]byte(value), &typed); err != nil {
			return nil, fmt.Errorf("expected a JSON VIP array with integer VRIDs")
		}
		var list []map[string]any
		if err := json.Unmarshal([]byte(value), &list); err != nil {
			return nil, fmt.Errorf("expected a JSON VIP array")
		}
		if list == nil {
			return nil, fmt.Errorf("expected a JSON VIP array")
		}
		return list, nil
	case "vip.preemptDelay":
		var delays map[string]int
		if err := json.Unmarshal([]byte(value), &delays); err != nil {
			return nil, fmt.Errorf("expected a JSON map of VIP delays")
		}
		if delays == nil {
			return nil, fmt.Errorf("expected a JSON map of VIP delays")
		}
		return delays, nil
	default:
		return coerce(key.Type, value)
	}
}

var supervisorVIPName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)
var supervisorInterface = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// Validate without reading credentials or discovering host interfaces. The
// existing renderers and supervisors still enforce their full runtime gates.
func (c *Config) validateSupervisors() []error {
	var errs []error
	for path, duration := range map[string]Duration{
		"edge.debounce": c.Edge.Debounce, "edge.drain": c.Edge.Drain,
		"edge.zonePoll": c.Edge.ZonePoll, "edge.stopTimeout": c.Edge.StopTimeout,
		"vip.stopTimeout": c.VIP.StopTimeout,
	} {
		if duration < 0 {
			errs = append(errs, fmt.Errorf("%s: must not be negative", path))
		}
	}
	names, vrids, addresses := map[string]bool{}, map[int]bool{}, map[netip.Addr]bool{}
	for i, vip := range c.VIP.VIPs {
		path := fmt.Sprintf("vip.vips[%d]", i)
		if !supervisorVIPName.MatchString(vip.Name) || names[vip.Name] {
			errs = append(errs, fmt.Errorf("%s.name: invalid or duplicate VIP name", path))
		}
		names[vip.Name] = true
		if !supervisorInterface.MatchString(vip.Interface) {
			errs = append(errs, fmt.Errorf("%s.interface: invalid interface", path))
		}
		if vrids[vip.VRID] {
			errs = append(errs, fmt.Errorf("%s.vrid: duplicate VRID", path))
		}
		vrids[vip.VRID] = true
		prefix, err := netip.ParsePrefix(vip.Address)
		if err != nil || prefix.Addr().IsUnspecified() || prefix.Addr().IsLoopback() || addresses[prefix.Addr()] {
			errs = append(errs, fmt.Errorf("%s.address: invalid or duplicate VIP prefix", path))
		}
		addresses[prefix.Addr()] = true
	}
	for name := range c.VIP.PreemptDelay {
		if !names[name] {
			errs = append(errs, fmt.Errorf("vip.preemptDelay[%s]: no such VIP", name))
		}
	}
	errs = append(errs, c.VIP.Observation.validate()...)
	if c.VIP.Observation.Provider == "keepalived" && (len(c.VIP.VIPs) == 0 || len(c.VIP.VIPs) > 32) {
		errs = append(errs, fmt.Errorf("vip.observation: keepalived requires 1-32 host vip.vips mappings"))
	}
	return errs
}

// Renderer spelling aliases accepted by the existing dnsdist renderer.
func supervisorRendererName(name string) string {
	switch strings.ToLower(name) {
	case "", "yml":
		return "yaml"
	default:
		return strings.ToLower(name)
	}
}

// VIPObservation mounts read-only ownership reporting in serve. It never starts
// a supervisor or permits weight, Service, allocation or cluster writes.
type VIPObservation struct {
	Provider           string              `koanf:"provider" json:"provider"`
	RunDir             string              `koanf:"runDir" json:"runDir"`
	Services           []VIPServiceBinding `koanf:"services" json:"services"`
	AnnouncerNamespace string              `koanf:"announcerNamespace" json:"announcerNamespace"`
	Interval           Duration            `koanf:"interval" json:"interval"`
	Timeout            Duration            `koanf:"timeout" json:"timeout"`
	MaxAge             Duration            `koanf:"maxAge" json:"maxAge"`
}

// VIPServiceBinding binds a configured VIP name/address to one exact Service.
type VIPServiceBinding struct {
	Name      string `koanf:"name" json:"name"`
	Address   string `koanf:"address" json:"address"`
	Namespace string `koanf:"namespace" json:"namespace"`
	Service   string `koanf:"service" json:"service"`
}

var vipServiceBindingShape = &Shape{Type: TypeObject, Fields: []Field{
	{Name: "name", Label: "VIP name", Help: "Local name for this observed VIP.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "address", Label: "Address", Help: "Expected Service LoadBalancer IP, used only to match allocation, never to infer ownership.", Required: true, Shape: Shape{Type: TypeString, Format: FormatIP}},
	{Name: "namespace", Label: "Service namespace", Help: "Exact namespace of the observed Service.", Required: true, Shape: Shape{Type: TypeString}},
	{Name: "service", Label: "Service name", Help: "Exact Service name. kube-vip observation requires its per-Service kubevip-<name> Lease.", Required: true, Shape: Shape{Type: TypeString}},
}}

func (v VIPObservation) validate() []error {
	var errs []error
	if len(v.Services) > 32 {
		errs = append(errs, fmt.Errorf("vip.observation.services: at most 32 bindings"))
	}
	names := map[string]bool{}
	for i, b := range v.Services {
		if !supervisorVIPName.MatchString(b.Name) || names[b.Name] {
			errs = append(errs, fmt.Errorf("vip.observation.services[%d].name: invalid or duplicate name", i))
		}
		names[b.Name] = true
		if len(validation.IsDNS1123Label(b.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(b.Service)) > 0 {
			errs = append(errs, fmt.Errorf("vip.observation.services[%d]: invalid Service name or namespace", i))
		}
	}
	if len(validation.IsDNS1123Label(v.AnnouncerNamespace)) > 0 {
		errs = append(errs, fmt.Errorf("vip.observation.announcerNamespace: invalid namespace"))
	}
	if v.Provider == "none" || v.Provider == "" {
		return errs
	}
	if v.Provider == "keepalived" && !filepath.IsAbs(v.RunDir) {
		errs = append(errs, fmt.Errorf("vip.observation.runDir: absolute local directory required"))
	}
	if v.Provider != "keepalived" && len(v.Services) == 0 {
		errs = append(errs, fmt.Errorf("vip.observation.services: at least one binding required"))
	}
	if v.Interval.Std() < time.Second || v.Interval.Std() > time.Minute || v.Timeout.Std() < 100*time.Millisecond || v.Timeout.Std() > 5*time.Second || v.MaxAge.Std() < v.Interval.Std()+v.Timeout.Std() || v.MaxAge.Std() > time.Minute {
		errs = append(errs, fmt.Errorf("vip.observation: interval must be 1s-1m, timeout 100ms-5s, maxAge at least interval+timeout and at most 1m"))
	}
	return errs
}
