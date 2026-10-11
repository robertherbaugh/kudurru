// Package edge holds the model shared by the dnsdist and keepalived
// supervisors (docs/architecture.md §3.6, D16-D20): the Topology that both
// dnsdist renderers (YAML, Lua) consume, the stable backend identity and
// the validation every rendered file goes through before dnsdist's own
// --check-config. The supervisors live in internal/edge/dnsdist and
// internal/edge/keepalived; the VIP abstraction in internal/edge/vip.
//
// dnsdist and keepalived are GPL-2.0 programs. kudurru only writes their
// configuration files and talks to their HTTP API and signals; nothing in
// this tree is derived from their source.
package edge

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// HealthQName is the name kudurru answers locally with TXT
// "<node-id> <serial-epoch>" (internal/stages/ingress.HealthName).
const HealthQName = "health.kudurru.internal."

// ProxyTLVSecret is the PROXYv2 TLV type carrying the shared secret
// (D45, D56; internal/dnsserver/proxyproto.TLVTypeSecret).
const ProxyTLVSecret = 0xE0

// Pool names rendered into every dnsdist configuration.
const (
	PoolResolver      = "resolver"
	PoolAuthoritative = "authoritative"
)

// BackendNamespace is the UUID namespace of backend ids: id =
// uuid5(BackendNamespace, node name). It is fixed forever; changing it
// would reshuffle every chashed ring at once.
var BackendNamespace = uuid.MustParse("6b1d3c4e-2f0a-5d8b-9c47-6b75647572ff")

// BackendID returns the stable dnsdist backend id of a kudurru node.
func BackendID(nodeName string) string {
	return uuid.NewSHA1(BackendNamespace, []byte(nodeName)).String()
}

// Protocol is a dnsdist frontend protocol.
type Protocol string

// Frontend protocols (dnsdist binds[].protocol).
const (
	Do53 Protocol = "Do53"
	DoT  Protocol = "DoT"
	DoH  Protocol = "DoH"
	DoQ  Protocol = "DoQ"
	DoH3 Protocol = "DoH3"
)

// AllProtocols lists the frontend protocols in rendering order.
var AllProtocols = []Protocol{Do53, DoT, DoH, DoQ, DoH3}

// TLS reports whether the protocol needs a certificate.
func (p Protocol) TLS() bool { return p != Do53 }

// DefaultPort is the well-known port of the protocol.
func (p Protocol) DefaultPort() int {
	switch p {
	case DoT, DoQ:
		return 853
	case DoH, DoH3:
		return 443
	}
	return 53
}

// ParseProtocol accepts the case-insensitive names do53, dot, doh, doq and
// doh3.
func ParseProtocol(s string) (Protocol, error) {
	for _, p := range AllProtocols {
		if strings.EqualFold(s, string(p)) {
			return p, nil
		}
	}
	return "", fmt.Errorf("edge: unknown protocol %q (want do53, dot, doh, doq or doh3)", s)
}

// Bind is one dnsdist frontend.
type Bind struct {
	// Address is host:port.
	Address  string
	Protocol Protocol
	// CertFile and KeyFile are required for every protocol but Do53.
	CertFile, KeyFile string
	// DoHPaths are the DoH/DoH3 URL paths; default /dns-query.
	DoHPaths []string
	// Threads is the number of listener threads (Do53 only); 0 = dnsdist
	// default.
	Threads int
	// ReusePort sets SO_REUSEPORT (Do53 only).
	ReusePort bool
}

// Policies dnsdist accepts for a pool (docs/research/dnsdist-keepalived.md
// §1.6).
var Policies = []string{"leastOutstanding", "firstAvailable", "wrandom", "whashed", "chashed", "roundrobin", "orderedWrandUntag"}

// PacketCache is a dnsdist packet cache attached to one pool.
type PacketCache struct {
	// Name is the cache object name; default "<pool>-cache".
	Name string
	// Size is the maximum number of entries.
	Size int
	// MaxTTL, MinTTL, StaleTTL, TemporaryFailureTTL and MaxNegativeTTL
	// are in seconds.
	MaxTTL, MinTTL, StaleTTL, TemporaryFailureTTL, MaxNegativeTTL int
}

// Pool is a dnsdist server pool.
type Pool struct {
	Name   string
	Policy string
	// Cache is nil for an uncached pool (the authoritative pool unless
	// edge.authoritativeCache is on).
	Cache *PacketCache
}

// Backend is one kudurru node behind dnsdist.
type Backend struct {
	// ID is BackendID(Name) when the node name is known, else
	// BackendID(Address).
	ID string
	// Name is the kudurru node name (pod name in Kubernetes); empty for an
	// anonymous static address.
	Name string
	// Address is host:port of the node's DNS listener (:5353 behind an
	// edge).
	Address string
	// InternalAddress is host:port of the node's internal listener
	// (:8082), polled for zone serials; empty disables polling of this
	// backend.
	InternalAddress string
	// Pools lists the pools the backend serves; every node serves all.
	Pools []string
	// ProxyProtocol sends a PROXYv2 header (always true for kudurru).
	ProxyProtocol bool
	// Weight spreads the chashed ring; default 100.
	Weight int
	// Order breaks leastOutstanding ties; default 1.
	Order int
	// Sockets is the number of UDP source sockets; default 2.
	Sockets int
}

// DisplayName is Name or, for anonymous static backends, the address.
func (b Backend) DisplayName() string {
	if b.Name != "" {
		return b.Name
	}
	return b.Address
}

// HealthCheck is the active probe dnsdist sends every backend.
type HealthCheck struct {
	QName string
	QType string
	// Interval is in seconds, Timeout in milliseconds.
	Interval, Timeout int
	MaxFailures       int
	Rise              int
	MustResolve       bool
	// ValidateNodeID renders the 2.1 validation function that requires the
	// TXT answer to start with the backend's node name, so a misrouted
	// address (a recycled pod IP now owned by another node) fails.
	// Anonymous static backends are only checked for NOERROR plus a TXT.
	ValidateNodeID bool
}

// Console is dnsdist's encrypted control socket.
type Console struct {
	// Address is a loopback host:port.
	Address string
	// Key is the base64 of 32 random bytes, generated per start.
	Key string
}

// Webserver is dnsdist's HTTP API.
type Webserver struct {
	// Address is a loopback host:port.
	Address string
	// APIKey is sent as X-API-Key; Password guards the dashboard. Both are
	// generated per start and rendered in plain text with
	// hash_plaintext_credentials so they never sit unhashed in dnsdist's
	// memory.
	APIKey, Password string
}

// Rules are the query rules rendered ahead of pool selection.
type Rules struct {
	// TransferSources may send AXFR/IXFR/NOTIFY/UPDATE; everything else
	// gets REFUSED at the edge. kudurru still applies its own
	// transferACL/notify policy to the PROXYv2-attributed address.
	TransferSources []string
	// AuthoritativeZones are routed to the authoritative pool; every other
	// query goes to the resolver pool.
	AuthoritativeZones []string
}

// DynBlocks is the opt-in dnsdist dynamic blocking group.
type DynBlocks struct {
	Enabled bool
	// QueryRate is queries per second per client prefix over Window
	// seconds; 0 disables the rule.
	QueryRate int
	// NXDomainRate is NXDOMAIN responses per second; 0 disables the rule.
	NXDomainRate int
	// Window is the measurement window in seconds; BlockFor the block
	// duration in seconds.
	Window, BlockFor int
	// Action is Drop, Refused, Truncate or NoRecurse.
	Action string
	// Exclude lists prefixes that are never blocked.
	Exclude []string
}

// EBPF is the opt-in kernel socket filter DynBlocks drops are pushed into.
type EBPF struct {
	Enabled                                   bool
	IPv4MaxItems, IPv6MaxItems, QNameMaxItems int
}

// Topology is everything a dnsdist configuration needs. Both renderers
// consume it; nothing else in the rendered file is configurable.
type Topology struct {
	// ServerID is general.server_id (the edge node or pod name).
	ServerID string
	// ACL lists client prefixes allowed to query.
	ACL         []string
	Binds       []Bind
	Pools       []Pool
	Backends    []Backend
	HealthCheck HealthCheck
	Console     Console
	Webserver   Webserver
	Rules       Rules
	DynBlocks   DynBlocks
	// ProxyProtocolSecret is the TLV 0xE0 value appended to every query;
	// empty only for loopback-only edges (D56).
	ProxyProtocolSecret string
	EBPF                EBPF
}

// DefaultACL is dnsdist's own default client ACL (private and loopback
// ranges); an edge on a public address must set ACL explicitly.
var DefaultACL = []string{"127.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.168.0.0/16", "172.16.0.0/12", "::1/128", "fc00::/7", "fe80::/10"}

// DefaultHealthCheck is the §3.6 probe: TXT health.kudurru.internal.,
// must_resolve, interval 1 s, 2 failures down, 2 successes up.
func DefaultHealthCheck() HealthCheck {
	return HealthCheck{QName: HealthQName, QType: "TXT", Interval: 1, Timeout: 1000, MaxFailures: 2, Rise: 2, MustResolve: true, ValidateNodeID: true}
}

// DefaultResolverCache is the resolver pool's packet cache.
func DefaultResolverCache() *PacketCache {
	return &PacketCache{Size: 200000, MaxTTL: 86400, MinTTL: 0, StaleTTL: 60, TemporaryFailureTTL: 60, MaxNegativeTTL: 3600}
}

// AuthoritativeCache is the short cache used when edge.authoritativeCache
// is on; the supervisor flushes it on every serial change.
func AuthoritativeCache(maxTTL int) *PacketCache {
	if maxTTL <= 0 {
		maxTTL = 30
	}
	return &PacketCache{Size: 20000, MaxTTL: maxTTL, MinTTL: 0, StaleTTL: 0, TemporaryFailureTTL: 5, MaxNegativeTTL: maxTTL}
}

// Credentials are generated per start.
type Credentials struct {
	ConsoleKey string
	APIKey     string
	Password   string
}

// NewCredentials returns fresh random credentials: a base64 32-byte
// console key and hex 32-byte API key and dashboard password.
func NewCredentials() (Credentials, error) {
	var b [3][32]byte
	for i := range b {
		if _, err := rand.Read(b[i][:]); err != nil {
			return Credentials{}, err
		}
	}
	return Credentials{
		ConsoleKey: base64.StdEncoding.EncodeToString(b[0][:]),
		APIKey:     hex.EncodeToString(b[1][:]),
		Password:   hex.EncodeToString(b[2][:]),
	}, nil
}

// Apply copies the credentials into the topology.
func (c Credentials) Apply(t *Topology) {
	t.Console.Key = c.ConsoleKey
	t.Webserver.APIKey = c.APIKey
	t.Webserver.Password = c.Password
}

// Normalize fills defaults and sorts backends so that equal topologies
// render byte-identical files.
func (t *Topology) Normalize() {
	if len(t.ACL) == 0 {
		t.ACL = slices.Clone(DefaultACL)
	}
	if t.HealthCheck.QName == "" {
		t.HealthCheck = DefaultHealthCheck()
	}
	for i := range t.Backends {
		b := &t.Backends[i]
		if b.ID == "" {
			if b.Name != "" {
				b.ID = BackendID(b.Name)
			} else {
				b.ID = BackendID(b.Address)
			}
		}
		if len(b.Pools) == 0 {
			for _, p := range t.Pools {
				b.Pools = append(b.Pools, p.Name)
			}
		}
		if b.Weight == 0 {
			b.Weight = 100
		}
		if b.Order == 0 {
			b.Order = 1
		}
		if b.Sockets == 0 {
			b.Sockets = 2
		}
	}
	sort.SliceStable(t.Backends, func(i, j int) bool {
		if t.Backends[i].Name != t.Backends[j].Name {
			return t.Backends[i].Name < t.Backends[j].Name
		}
		return t.Backends[i].Address < t.Backends[j].Address
	})
	for i := range t.Pools {
		if c := t.Pools[i].Cache; c != nil && c.Name == "" {
			c.Name = t.Pools[i].Name + "-cache"
		}
	}
	for i := range t.Binds {
		b := &t.Binds[i]
		if (b.Protocol == DoH || b.Protocol == DoH3) && len(b.DoHPaths) == 0 {
			b.DoHPaths = []string{"/dns-query"}
		}
	}
	t.Rules.AuthoritativeZones = NormalizeZones(t.Rules.AuthoritativeZones)
	if t.DynBlocks.Enabled {
		if t.DynBlocks.Window == 0 {
			t.DynBlocks.Window = 10
		}
		if t.DynBlocks.BlockFor == 0 {
			t.DynBlocks.BlockFor = 60
		}
		if t.DynBlocks.Action == "" {
			t.DynBlocks.Action = "Drop"
		}
	}
	if t.EBPF.Enabled {
		if t.EBPF.IPv4MaxItems == 0 {
			t.EBPF.IPv4MaxItems = 1024
		}
		if t.EBPF.IPv6MaxItems == 0 {
			t.EBPF.IPv6MaxItems = 1024
		}
		if t.EBPF.QNameMaxItems == 0 {
			t.EBPF.QNameMaxItems = 1024
		}
	}
}

// NormalizeZones lower-cases, fully qualifies, de-duplicates and sorts
// zone names, dropping the root (forwarded traffic is resolver traffic).
func NormalizeZones(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, z := range in {
		z = strings.ToLower(strings.TrimSpace(z))
		if z == "" || z == "." {
			continue
		}
		if !strings.HasSuffix(z, ".") {
			z += "."
		}
		if !seen[z] {
			seen[z] = true
			out = append(out, z)
		}
	}
	sort.Strings(out)
	return out
}

// Pool returns the named pool.
func (t *Topology) Pool(name string) (Pool, bool) {
	for _, p := range t.Pools {
		if p.Name == name {
			return p, true
		}
	}
	return Pool{}, false
}

// Validate reports every problem with the topology. Renderers call it, so
// an invalid topology never reaches dnsdist.
func (t *Topology) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(t.Binds) == 0 {
		add("no binds")
	}
	for _, b := range t.Binds {
		if err := validHostPort(b.Address); err != nil {
			add("bind %s: %v", b.Address, err)
		}
		if _, err := ParseProtocol(string(b.Protocol)); err != nil {
			add("bind %s: %v", b.Address, err)
		}
		if b.Protocol.TLS() && (b.CertFile == "" || b.KeyFile == "") {
			add("bind %s (%s): certificate and key files are required", b.Address, b.Protocol)
		}
	}
	pools := map[string]bool{}
	for _, p := range t.Pools {
		if p.Name == "" {
			add("pool with empty name")
		}
		if pools[p.Name] {
			add("pool %q declared twice", p.Name)
		}
		pools[p.Name] = true
		if !slices.Contains(Policies, p.Policy) {
			add("pool %q: unknown policy %q (want one of %s)", p.Name, p.Policy, strings.Join(Policies, ", "))
		}
		if p.Cache != nil && p.Cache.Size <= 0 {
			add("pool %q: packet cache size must be positive", p.Name)
		}
	}
	if !pools[PoolResolver] {
		add("pool %q is required", PoolResolver)
	}
	if len(t.Rules.AuthoritativeZones) > 0 && !pools[PoolAuthoritative] {
		add("authoritative zones are routed but pool %q is missing", PoolAuthoritative)
	}
	ids := map[string]string{}
	for _, b := range t.Backends {
		if err := validHostPort(b.Address); err != nil {
			add("backend %s: %v", b.DisplayName(), err)
		}
		if prev, dup := ids[b.ID]; dup {
			add("backends %s and %s share id %s", prev, b.DisplayName(), b.ID)
		}
		ids[b.ID] = b.DisplayName()
		for _, p := range b.Pools {
			if !pools[p] {
				add("backend %s: unknown pool %q", b.DisplayName(), p)
			}
		}
		if !safeName(b.Name) {
			add("backend name %q may only contain letters, digits, '.', '-' and '_'", b.Name)
		}
	}
	if !safeName(t.ServerID) {
		add("server id %q may only contain letters, digits, '.', '-' and '_'", t.ServerID)
	}
	if err := validLoopback(t.Console.Address); err != nil {
		add("console: %v", err)
	}
	if raw, err := base64.StdEncoding.DecodeString(t.Console.Key); err != nil || len(raw) != 32 {
		add("console: key must be the base64 of 32 bytes")
	}
	if err := validLoopback(t.Webserver.Address); err != nil {
		add("webserver: %v", err)
	}
	if !safeName(t.Webserver.APIKey) || !safeName(t.Webserver.Password) || t.Webserver.APIKey == "" || t.Webserver.Password == "" {
		add("webserver: api key and password are required and must be [A-Za-z0-9._-]")
	}
	hc := t.HealthCheck
	if hc.Interval <= 0 || hc.MaxFailures <= 0 || hc.Rise <= 0 || hc.Timeout <= 0 {
		add("health check: interval, timeout, max failures and rise must be positive")
	}
	for _, n := range t.Rules.TransferSources {
		if _, err := netip.ParsePrefix(n); err != nil {
			if _, err := netip.ParseAddr(n); err != nil {
				add("transfer source %q is neither an address nor a prefix", n)
			}
		}
	}
	for _, z := range t.Rules.AuthoritativeZones {
		if !safeName(strings.TrimSuffix(z, ".")) {
			add("zone %q contains characters outside [A-Za-z0-9._-]", z)
		}
	}
	for _, n := range append(slices.Clone(t.ACL), t.DynBlocks.Exclude...) {
		if _, err := netip.ParsePrefix(n); err != nil {
			add("prefix %q: %v", n, err)
		}
	}
	if s := t.ProxyProtocolSecret; s != "" {
		if err := ValidSecret([]byte(s)); err != nil {
			add("proxy protocol secret: %v", err)
		}
	}
	if t.DynBlocks.Enabled {
		switch t.DynBlocks.Action {
		case "Drop", "Refused", "Truncate", "NoRecurse":
		default:
			add("dynblocks: unknown action %q", t.DynBlocks.Action)
		}
		if t.DynBlocks.QueryRate == 0 && t.DynBlocks.NXDomainRate == 0 {
			add("dynblocks: enabled without a query or NXDOMAIN rate")
		}
	}
	return errors.Join(errs...)
}

// MinSecretLen is the shortest accepted PROXYv2 secret
// (internal/dnsserver/proxyproto.MinSecretLen).
const MinSecretLen = 32

// ValidSecret checks a PROXYv2 TLV secret: at least 32 bytes of printable
// ASCII without spaces or quotes. Raw binary secrets cannot be expressed in
// a YAML string (YAML escapes are code points, not bytes, so dnsdist would
// send their UTF-8 encoding), so the supervisor requires a hex or base64
// secret (`openssl rand -hex 32`).
func ValidSecret(b []byte) error {
	if len(b) < MinSecretLen {
		return fmt.Errorf("%d bytes, need at least %d", len(b), MinSecretLen)
	}
	for _, c := range b {
		if c < 0x21 || c > 0x7e || c == '"' || c == '\'' || c == '\\' {
			return errors.New("must be printable ASCII without spaces, quotes or backslashes (generate it with `openssl rand -hex 32`)")
		}
	}
	return nil
}

// safeName reports whether s is empty or made of [A-Za-z0-9._-] only: the
// characters that need no quoting in YAML, Lua or keepalived.conf.
func safeName(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func validHostPort(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}
	if host == "" {
		return errors.New("empty host")
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func validLoopback(s string) error {
	if err := validHostPort(s); err != nil {
		return fmt.Errorf("%q: %w", s, err)
	}
	host, _, _ := net.SplitHostPort(s)
	a, err := netip.ParseAddr(host)
	if err != nil || !a.IsLoopback() {
		return fmt.Errorf("%q must be a loopback address (credentials are per start and the API has no TLS)", s)
	}
	return nil
}
