// Package config loads kudurru.yaml plus KUDURRU_<SECTION>__<KEY> environment
// overrides through koanf, and owns the settings metadata table that drives
// the file loader, the env mapping, `kudurru config`, the API, the UI and
// the docs (docs/architecture.md §6.2, decision D24).
package config

import (
	"sort"
	"strings"
)

// Type is the value type of a settings key.
type Type string

// Value types.
const (
	TypeString     Type = "string"
	TypeInt        Type = "int"
	TypeBool       Type = "bool"
	TypeDuration   Type = "duration"
	TypeBytes      Type = "bytes"
	TypeStringList Type = "string[]"
	TypeList       Type = "list"
	TypeMap        Type = "map"
)

// Scope says whether a key is host-local or replicated cluster-wide.
type Scope string

// Scopes.
const (
	ScopeNode    Scope = "node"
	ScopeCluster Scope = "cluster"
)

// Key is one settings key and its metadata.
type Key struct {
	Path    string
	Type    Type
	Scope   Scope
	Default any
	Help    string
	// HostOnly keys can only be set through the file or environment on the
	// host, never through the API or UI (the Pi-hole 2026 RCE class made
	// structural).
	HostOnly bool
	// EnvLocked is set at load time when the environment supplied the value;
	// the UI then shows the key read-only.
	EnvLocked bool
	// RestartRequired keys roll pods in Kubernetes (checksum annotation on
	// the node ConfigMap) and need a process restart in Docker. Every other
	// key is applied without a restart (config.Reloadable subscribers in
	// serve: upstreams, cache, blocking defaults, ratelimit, querylog
	// privacy, exclusions and retention, log.level); TestRestartRequired
	// pins the hot set.
	RestartRequired bool
	// WriteOnly keys are never read back through the API (secrets).
	WriteOnly bool

	// Label is the human name of the key ("Default block response"); the
	// path stays the key. Group is the UI section it belongs to.
	Label string
	Group string

	// The typed-control metadata (shape.go): the closed set of a string
	// key, the bounds of an int key, suggested values, the syntax of a
	// string, the kind of object it names, and the shape of the elements
	// of a list (Items) or of the keys and values of a map (Keys, Values).
	// Validate enforces it; GET /settings/_schema serves it.
	Enum    []Option
	Min     *int
	Max     *int
	Presets []string
	Format  Format
	Ref     Ref
	Items   *Shape
	Keys    *Shape
	Values  *Shape
	Fields  []Field
	// normalize maps the value before the enum test (Shape.normalize).
	normalize func(string) string
}

// UI sections (Key.Group), in display order.
const (
	GroupNode         = "Node"
	GroupListeners    = "Listeners"
	GroupUpstreams    = "Upstreams"
	GroupCache        = "Cache"
	GroupBlocking     = "Blocking"
	GroupBlockPage    = "Block page"
	GroupQuerylog     = "Query log"
	GroupRateLimit    = "Rate limiting"
	GroupSecurity     = "Security"
	GroupZones        = "Zone transfers"
	GroupIntegrations = "Integrations"
	GroupLogging      = "Logging"
)

// Groups lists the UI sections in display order.
var Groups = []string{
	GroupNode, GroupListeners, GroupUpstreams, GroupCache, GroupBlocking, GroupBlockPage, GroupQuerylog,
	GroupRateLimit, GroupSecurity, GroupZones, GroupIntegrations, GroupLogging,
}

// Closed sets shared by several keys.
var (
	// BlockResponses are the values of blocking.defaultResponse (the
	// BlockResponse values of api/v1alpha1).
	BlockResponses = []Option{
		{Value: "zeroIP", Label: "Zero IP", Help: "0.0.0.0 for A, :: for AAAA; other types NODATA."},
		{Value: "nxdomain", Label: "NXDOMAIN", Help: "The name does not exist."},
		{Value: "customIP", Label: "Custom IP", Help: "The addresses in blocking.customIP.v4 and v6; NODATA for a family without one."},
		{Value: "refused", Label: "REFUSED", Help: "Discouraged: stub resolvers fail over to another server."},
		{Value: "blockPage", Label: "Block page", Help: "A and AAAA point at the block page, which tells a browser why the name was blocked; other types NODATA."},
	}
	upstreamStrategies = []Option{
		{Value: "parallel_best", Label: "Parallel best", Help: "Race two healthy servers picked at random and take the first usable answer; if both fail, race the rest."},
		{Value: "strict", Label: "Strict order", Help: "Ask the servers one after another in the listed order, moving on only on failure."},
		{Value: "random", Label: "Random", Help: "Ask one healthy server picked at random; another random one on failure."},
		{Value: "rtt_weighted", Label: "Fastest (RTT weighted)", Help: "Pick the first server at random weighted by its inverse round-trip time, then fall back in RTT order."},
	}
	dnsProtocols = []Option{
		{Value: "udp", Label: "UDP"},
		{Value: "tcp", Label: "TCP"},
	}
	tsigAlgorithms = []Option{
		{Value: "hmac-sha256", Label: "HMAC-SHA256"},
		{Value: "hmac-sha512", Label: "HMAC-SHA512"},
	}
	// rbacRoles are the roles of internal/rbac/permissions.yaml
	// (TestRBACRolesMatchPermissions).
	rbacRoles = []Option{
		{Value: "viewer", Label: "Viewer", Help: "Read zones, records, lists, policies, stats and settings."},
		{Value: "auditor", Label: "Auditor", Help: "Viewer plus the query log and the audit log."},
		{Value: "operator", Label: "Operator", Help: "Manage records, lists, policies, groups, clients and blocking; flush the cache."},
		{Value: "zone-admin", Label: "Zone admin", Help: "Manage zones and their records, transfers and signing."},
		{Value: "admin", Label: "Admin", Help: "Everything."},
	}
	bindingScopes = []Option{
		{Value: "cluster", Label: "Cluster"},
		{Value: "zone", Label: "Zone"},
		{Value: "zonegroup", Label: "Zone group"},
		{Value: "clientgroup", Label: "Client group"},
	}
	subjectKinds = []Option{
		{Value: "User", Label: "User"},
		{Value: "Group", Label: "Group"},
		{Value: "ServiceAccount", Label: "Service account"},
	}
)

// Shapes of the list and map keys.
var (
	tlsFilesShape = Shape{Type: TypeObject, Fields: []Field{
		{Name: "certFile", Label: "Certificate file", Help: "PEM certificate chain; reloaded when it changes.", Required: true, Shape: Shape{Type: TypeString}},
		{Name: "keyFile", Label: "Key file", Help: "PEM private key of certFile.", Required: true, Shape: Shape{Type: TypeString}},
	}}
	listenerAddress  = Field{Name: "address", Label: "Address", Help: "host:port to bind (0.0.0.0:53, [::]:53, :53).", Required: true, Shape: Shape{Type: TypeString, Format: FormatHostPort}}
	dnsListenerShape = &Shape{Type: TypeObject, Fields: []Field{
		listenerAddress,
		{Name: "protocols", Label: "Protocols", Help: "Empty serves both.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Enum: dnsProtocols}, Default: []string{"udp", "tcp"}}},
	}}
	tlsListenerShape = &Shape{Type: TypeObject, Fields: []Field{
		listenerAddress,
		{Name: "tls", Label: "TLS", Required: true, Shape: tlsFilesShape},
	}}
	httpListenerShape = &Shape{Type: TypeObject, Fields: []Field{
		listenerAddress,
		{Name: "path", Label: "Path", Help: "URL path of the endpoint; starts with /.", Shape: Shape{Type: TypeString, Default: "/dns-query"}},
		{Name: "tls", Label: "TLS", Required: true, Shape: tlsFilesShape},
	}}
	upstreamGroupShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "servers", Label: "Servers", Help: "ip[:port], udp://, tcp://, tls://, https://, quic://, h3:// or sdns:// stamps; prefix a display name with name= (cloudflare=tls://1.1.1.1).", Required: true,
			Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatUpstream}}},
		{Name: "strategy", Label: "Strategy", Help: "Which servers are asked, and in which order.", Shape: Shape{Type: TypeString, Enum: upstreamStrategies, Default: "parallel_best"}},
		{Name: "timeout", Label: "Timeout", Help: "Bound of one exchange with one server.", Shape: Shape{Type: TypeDuration, Presets: []string{"500ms", "1s", "2s", "5s"}, Default: "2s"}},
	}}
	tsigKeyShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "name", Label: "Name", Help: "Key name (a domain name) the zones reference in spec.policy.transferTSIGKeyRefs.", Required: true, Shape: Shape{Type: TypeString, Format: FormatDomain}},
		{Name: "algorithm", Label: "Algorithm", Required: true, Shape: Shape{Type: TypeString, Enum: tsigAlgorithms, normalize: func(v string) string {
			return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
		}}},
		{Name: "secretFile", Label: "Secret file", Help: "File holding the base64 secret (tsig-keygen format).", Required: true, Shape: Shape{Type: TypeString}},
	}}
	identityProviderShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "id", Label: "ID", Help: "Provider id (a DNS label); the default username and group prefix is <id>:.", Required: true, Shape: Shape{Type: TypeString}},
		{Name: "displayName", Label: "Display name", Shape: Shape{Type: TypeString}},
		{Name: "issuer", Label: "Issuer", Help: "OpenID Provider issuer URL (https).", Shape: Shape{Type: TypeString, Format: FormatURL}},
		{Name: "clientID", Label: "Client ID", Shape: Shape{Type: TypeString}},
		{Name: "clientSecretFile", Label: "Client secret file", Shape: Shape{Type: TypeString}},
		{Name: "cliClientID", Label: "CLI client ID", Shape: Shape{Type: TypeString}},
		{Name: "redirectURL", Label: "Redirect URL", Help: "Absolute callback URL registered at the provider.", Shape: Shape{Type: TypeString, Format: FormatURL}},
		{Name: "scopes", Label: "Scopes", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString}}},
		{Name: "audiences", Label: "Audiences", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString}}},
		{Name: "claimValidationRules", Label: "Claim validation rules", Shape: Shape{Type: TypeList, Items: &Shape{Type: TypeObject}}},
		{Name: "claimMappings", Label: "Claim mappings", Shape: Shape{Type: TypeObject}},
		{Name: "provisioning", Label: "Provisioning", Shape: Shape{Type: TypeObject}},
		{Name: "allowedDomains", Label: "Allowed email domains", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatDomain}}},
	}}
	roleBindingShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "role", Label: "Role", Required: true, Shape: Shape{Type: TypeString, Enum: rbacRoles}},
		{Name: "scope", Label: "Scope", Shape: Shape{Type: TypeObject, Fields: []Field{
			{Name: "kind", Label: "Kind", Shape: Shape{Type: TypeString, Enum: bindingScopes, Default: "cluster"}},
			{Name: "name", Label: "Name", Help: "Zone, zone group or client group name; empty for cluster scope.", Shape: Shape{Type: TypeString}},
		}}},
		{Name: "subjects", Label: "Subjects", Required: true, Shape: Shape{Type: TypeList, Items: &Shape{Type: TypeObject, Fields: []Field{
			{Name: "kind", Label: "Kind", Required: true, Shape: Shape{Type: TypeString, Enum: subjectKinds}},
			{Name: "name", Label: "Name", Required: true, Shape: Shape{Type: TypeString}},
			{Name: "namespace", Label: "Namespace", Help: "Required for a ServiceAccount.", Shape: Shape{Type: TypeString}},
		}}}},
	}}
	objectShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "apiVersion", Label: "API version", Shape: Shape{Type: TypeString}},
		{Name: "kind", Label: "Kind", Shape: Shape{Type: TypeString}},
		{Name: "metadata", Label: "Metadata", Shape: Shape{Type: TypeObject}},
		{Name: "spec", Label: "Spec", Shape: Shape{Type: TypeObject}},
	}}
	rateLevelValue = &Shape{Type: TypeInt, Min: new(0)}
)

// Schema is the settings metadata table. Every leaf of Config must have an
// entry; TestSchemaMatchesConfig enforces it.
var Schema = []Key{
	{Path: "node.name", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Node identity; defaults to the hostname.", HostOnly: true, RestartRequired: true, Label: "Node name", Group: GroupNode},
	{Path: "node.lameduck", Type: TypeDuration, Scope: ScopeNode, Default: "2s", Help: "Drain window after SIGTERM: readiness answers 503 and the health name SERVFAIL for this long before the listeners close (0 disables). The Helm chart also runs kudurru lameduck in preStop.", HostOnly: true, RestartRequired: true, Label: "Lameduck drain window", Group: GroupNode, Presets: []string{"0s", "2s", "5s", "10s"}},
	{Path: "node.dataDir", Type: TypeString, Scope: ScopeNode, Default: "/var/lib/kudurru", Help: "Directory for state.db, querylog.db, local.db, lists and keys.", HostOnly: true, RestartRequired: true, Label: "Data directory", Group: GroupNode},
	{Path: "listeners.dns", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{{"address": "0.0.0.0:53", "protocols": []string{"udp", "tcp"}}}, Help: "DNS listeners: address and protocols (udp, tcp).", HostOnly: true, RestartRequired: true, Label: "DNS listeners", Group: GroupListeners, Items: dnsListenerShape},
	{Path: "listeners.dot", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-TLS listeners (RFC 7858, ALPN dot): address and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true, Label: "DNS-over-TLS listeners", Group: GroupListeners, Items: tlsListenerShape},
	{Path: "listeners.doh", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-HTTPS listeners (RFC 8484, HTTP/2 and HTTP/1.1): address, path (default /dns-query) and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true, Label: "DNS-over-HTTPS listeners", Group: GroupListeners, Items: httpListenerShape},
	{Path: "listeners.doq", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-QUIC listeners (RFC 9250, udp, ALPN doq): address and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true, Label: "DNS-over-QUIC listeners", Group: GroupListeners, Items: tlsListenerShape},
	{Path: "listeners.doh3", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-HTTPS over HTTP/3 listeners (udp, ALPN h3): address, path (default /dns-query) and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true, Label: "DNS-over-HTTP/3 listeners", Group: GroupListeners, Items: httpListenerShape},
	{Path: "listeners.api.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8080", Help: "HTTP API and UI listener.", HostOnly: true, RestartRequired: true, Label: "API and UI address", Group: GroupListeners, Format: FormatHostPort},
	{Path: "listeners.api.tls.auto", Type: TypeString, Scope: ScopeNode, Default: "selfsigned", Help: "TLS for the API listener when no certFile is set: selfsigned (generate a certificate once into <node.dataDir>/keys/) or off (plain HTTP, e.g. behind a TLS-terminating proxy).", HostOnly: true, RestartRequired: true, Label: "API automatic TLS", Group: GroupListeners, Enum: []Option{{Value: "selfsigned", Label: "Self-signed", Help: "Generate a certificate once into <node.dataDir>/keys/."}, {Value: "off", Label: "Off (plain HTTP)", Help: "For a TLS-terminating reverse proxy in front."}}},
	{Path: "listeners.api.tls.certFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "API listener certificate (PEM); takes precedence over tls.auto.", HostOnly: true, RestartRequired: true, Label: "API certificate file", Group: GroupListeners},
	{Path: "listeners.api.tls.keyFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Private key for listeners.api.tls.certFile.", HostOnly: true, RestartRequired: true, Label: "API key file", Group: GroupListeners},
	{Path: "listeners.api.pathPrefix", Type: TypeString, Scope: ScopeNode, Default: "", Help: "URL path prefix the API and UI are served under when a reverse proxy mounts kudurru below / (e.g. /kudurru).", HostOnly: true, RestartRequired: true, Label: "API path prefix", Group: GroupListeners},
	{Path: "listeners.api.trustedProxies", Type: TypeStringList, Scope: ScopeNode, Default: []string{}, Help: "Reverse proxies (addresses or CIDRs) whose X-Forwarded-For and X-Forwarded-Proto headers the API trusts for the client address and scheme.", HostOnly: true, RestartRequired: true, Label: "Trusted reverse proxies", Group: GroupListeners, Items: &Shape{Type: TypeString, Format: FormatCIDR}},
	{Path: "listeners.ops.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8081", Help: "Unauthenticated health listener (/healthz/*).", HostOnly: true, RestartRequired: true, Label: "Health listener address", Group: GroupListeners, Format: FormatHostPort},
	{Path: "listeners.metrics.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:9153", Help: "Prometheus /metrics listener.", HostOnly: true, RestartRequired: true, Label: "Metrics listener address", Group: GroupListeners, Format: FormatHostPort},
	{Path: "listeners.proxyProtocolFrom", Type: TypeStringList, Scope: ScopeNode, Default: []string{}, Help: "Exact peers (CIDRs, normally /32) allowed to send PROXYv2 headers; loopback only when dnsdist shares the network namespace.", HostOnly: true, RestartRequired: true, Label: "PROXYv2 peers", Group: GroupListeners, Items: &Shape{Type: TypeString, Format: FormatCIDR}},
	{Path: "listeners.proxyProtocolSecretFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "File holding the PROXYv2 TLV 0xE0 secret shared with the dnsdist supervisor (at least 32 bytes, e.g. openssl rand -hex 32); required when proxyProtocolFrom names a non-loopback peer.", HostOnly: true, RestartRequired: true, Label: "PROXYv2 secret file", Group: GroupListeners},
	{Path: "listeners.internal.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8082", Help: "Node-to-node listener (list bodies by digest, /internal/status); always TLS.", HostOnly: true, RestartRequired: true, Label: "Node-to-node listener address", Group: GroupListeners, Format: FormatHostPort},
	{Path: "listeners.internal.tokenFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "File holding the internal bearer token; empty derives it from the encryption key (KUDURRU_ENCRYPTION_KEY) with HKDF.", HostOnly: true, RestartRequired: true, Label: "Internal token file", Group: GroupListeners},
	{Path: "listeners.internal.tls.certFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Internal listener certificate (PEM); empty issues one from the CA derived from the encryption key.", HostOnly: true, RestartRequired: true, Label: "Internal certificate file", Group: GroupListeners},
	{Path: "listeners.internal.tls.keyFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Private key for listeners.internal.tls.certFile.", HostOnly: true, RestartRequired: true, Label: "Internal key file", Group: GroupListeners},
	{Path: "listeners.internal.tls.caFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "CA bundle (PEM) trusted when dialling other nodes; empty trusts the CA derived from the encryption key.", HostOnly: true, RestartRequired: true, Label: "Internal CA bundle", Group: GroupListeners},
	{Path: "listeners.internal.tls.serverName", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Name verified in peers' internal certificates; empty uses the derived certificates' fixed name. Set it to a SAN of tls.certFile.", HostOnly: true, RestartRequired: true, Label: "Internal TLS server name", Group: GroupListeners, Format: FormatDomain},
	{Path: "listeners.blockPage.http", Type: TypeString, Scope: ScopeNode, Default: ":80", Help: "Plain-HTTP listener of the block page (blockPage.enabled); empty disables it. Browsers that resolve a blocked name under the blockPage response connect here.", HostOnly: true, RestartRequired: true, Label: "Block page HTTP address", Group: GroupBlockPage, Format: FormatHostPort, Presets: []string{":80", "0.0.0.0:80", "[::]:80"}},
	{Path: "listeners.blockPage.https", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Optional HTTPS listener of the block page (e.g. :443). No certificate can be valid for the blocked names, so browsers show a certificate warning there, and HSTS sites cannot be bypassed at all (docs/deploy/blocking-page.md); empty disables it.", HostOnly: true, RestartRequired: true, Label: "Block page HTTPS address", Group: GroupBlockPage, Format: FormatHostPort, Presets: []string{":443"}},
	{Path: "listeners.blockPage.tls.certFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Certificate (PEM) of listeners.blockPage.https; empty generates a self-signed one into <node.dataDir>/keys/blockpage.crt.", HostOnly: true, RestartRequired: true, Label: "Block page certificate file", Group: GroupBlockPage},
	{Path: "listeners.blockPage.tls.keyFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Private key for listeners.blockPage.tls.certFile.", HostOnly: true, RestartRequired: true, Label: "Block page key file", Group: GroupBlockPage},
	{Path: "listeners.sockets", Type: TypeInt, Scope: ScopeNode, Default: 0, Help: "SO_REUSEPORT sockets per udp and tcp DNS listener, each with its own read loop (Linux; one elsewhere); 0 means GOMAXPROCS, 1 a single socket.", HostOnly: true, RestartRequired: true, Label: "Sockets per DNS listener", Group: GroupListeners, Min: new(0)},
	{Path: "store.driver", Type: TypeString, Scope: ScopeNode, Default: "sqlite", Help: "Store driver: sqlite (state.db under node.dataDir) or kube (CRDs via the apiserver).", HostOnly: true, RestartRequired: true, Label: "Store driver", Group: GroupNode, Enum: []Option{{Value: "sqlite", Label: "SQLite", Help: "state.db under node.dataDir."}, {Value: "kube", Label: "Kubernetes", Help: "CRDs through the apiserver."}}},
	{Path: "store.kube.startupTimeout", Type: TypeDuration, Scope: ScopeNode, Default: "15s", Help: "How long to wait for the apiserver watches to sync before serving from local.db.", HostOnly: true, RestartRequired: true, Label: "Kubernetes sync timeout", Group: GroupNode, Presets: []string{"5s", "15s", "30s", "1m"}},
	{Path: "store.kube.snapshot.maxAge", Type: TypeDuration, Scope: ScopeNode, Default: "24h", Help: "Oldest local.db snapshot that may still answer primary zones; older snapshots answer SERVFAIL while forwarding continues.", HostOnly: true, RestartRequired: true, Label: "Snapshot maximum age", Group: GroupNode, Presets: []string{"1h", "24h", "168h"}},
	{Path: "upstreams.groups", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"default": map[string]any{"servers": []string{"1.1.1.1:53", "9.9.9.9:53"}, "strategy": "parallel_best", "timeout": "2s"}}, Help: "Named upstream groups: servers, strategy (parallel_best, strict, random, rtt_weighted), timeout.", Label: "Upstream groups", Group: GroupUpstreams, Keys: &Shape{Type: TypeString}, Values: upstreamGroupShape},
	{Path: "upstreams.bootstrap", Type: TypeStringList, Scope: ScopeCluster, Default: []string{"1.1.1.1:53"}, Help: "Plain resolvers used to resolve upstream hostnames (DoT/DoH).", Label: "Bootstrap resolvers", Group: GroupUpstreams, Items: &Shape{Type: TypeString, Format: FormatUpstream}},
	{Path: "upstreams.conditional", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{}, Help: "Suffix to upstream group map for conditional forwarding (longest suffix wins); a key may list several suffixes in dnsmasq form, \"[/corp.example/10.in-addr.arpa/]\". Conditional answers are never cached.", Label: "Conditional forwarding", Group: GroupUpstreams, Keys: &Shape{Type: TypeString}, Values: &Shape{Type: TypeString, Ref: RefUpstreamGroup}},
	{Path: "upstreams.ecs.mode", Type: TypeString, Scope: ScopeCluster, Default: "strip", Help: "EDNS Client Subnet on upstream queries: strip (never send one; privacy default), forward (pass the client's own option), add (forward the client's option, else derive one from a public client address).", Label: "EDNS Client Subnet", Group: GroupUpstreams, Enum: []Option{{Value: "strip", Label: "Strip", Help: "Never send a client subnet (privacy default)."}, {Value: "forward", Label: "Forward", Help: "Pass the client's own ECS option, truncated to the prefix limits."}, {Value: "add", Label: "Add", Help: "Forward the client's option, else derive one from a public client address."}}},
	{Path: "upstreams.ecs.ipv4Prefix", Type: TypeInt, Scope: ScopeCluster, Default: 24, Help: "Longest IPv4 source prefix sent upstream in ECS (0-32).", Label: "ECS IPv4 prefix length", Group: GroupUpstreams, Min: new(0), Max: new(32)},
	{Path: "upstreams.ecs.ipv6Prefix", Type: TypeInt, Scope: ScopeCluster, Default: 56, Help: "Longest IPv6 source prefix sent upstream in ECS (0-128).", Label: "ECS IPv6 prefix length", Group: GroupUpstreams, Min: new(0), Max: new(128)},
	{Path: "cache.maxEntries", Type: TypeInt, Scope: ScopeCluster, Default: 200000, Help: "Maximum number of cached responses (LRU beyond it); 0 disables the cache.", Label: "Maximum cache entries", Group: GroupCache, Min: new(0)},
	{Path: "cache.minTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "5s", Help: "Lower clamp for positive answer TTLs.", Label: "Minimum TTL", Group: GroupCache, Presets: []string{"0s", "5s", "30s", "1m", "5m"}},
	{Path: "cache.maxTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "Upper clamp for answer TTLs.", Label: "Maximum TTL", Group: GroupCache, Presets: []string{"1h", "6h", "24h", "168h"}},
	{Path: "cache.negativeTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "5m", Help: "Upper clamp for NXDOMAIN/NODATA TTLs (RFC 2308 SOA minimum otherwise).", Label: "Maximum negative TTL", Group: GroupCache, Presets: []string{"30s", "1m", "5m", "1h"}},
	{Path: "cache.prefetch.threshold", Type: TypeInt, Scope: ScopeCluster, Default: 5, Help: "Hits within prefetch.window that make an entry hot enough to refresh before it expires; 0 disables prefetch.", Label: "Prefetch threshold (hits)", Group: GroupCache, Min: new(0)},
	{Path: "cache.prefetch.window", Type: TypeDuration, Scope: ScopeCluster, Default: "2h", Help: "Window over which prefetch hits are counted.", Label: "Prefetch window", Group: GroupCache, Presets: []string{"30m", "1h", "2h", "6h"}},
	{Path: "cache.serveStale.ttl", Type: TypeDuration, Scope: ScopeCluster, Default: "72h", Help: "How long past expiry an answer may be served stale when every upstream fails (RFC 8767); 0 disables serve-stale.", Label: "Serve stale for", Group: GroupCache, Presets: []string{"0s", "1h", "24h", "72h"}},
	{Path: "cache.serveStale.answerTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "30s", Help: "TTL of stale answers (RFC 8767).", Label: "Stale answer TTL", Group: GroupCache, Presets: []string{"5s", "30s", "1m"}},
	{Path: "cache.serveStale.maxWait", Type: TypeDuration, Scope: ScopeCluster, Default: "1800ms", Help: "How long a query for an expired entry waits for the upstream refresh before the stale answer is sent (RFC 8767 client response timer).", Label: "Stale answer wait", Group: GroupCache, Presets: []string{"0s", "500ms", "1800ms", "3s"}},
	{Path: "blocking.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Apply blocklists to recursive queries; false answers everything unfiltered (temporary disable is a runtime toggle, not this key).", Label: "Blocking enabled", Group: GroupBlocking},
	{Path: "blocking.defaultResponse", Type: TypeString, Scope: ScopeCluster, Default: "zeroIP", Help: "Block response when neither the Policy (blockMode) nor the BlockList (response) sets one: zeroIP (0.0.0.0 / ::, other types NODATA), nxdomain, customIP (blocking.customIP), refused (discouraged: stub resolvers fail over), blockPage (A and AAAA answer the block page address, other types NODATA).", Label: "Default block response", Group: GroupBlocking, Enum: BlockResponses},
	{Path: "blocking.blockTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "10m", Help: "TTL of block responses when the Policy sets no blockTTL; short so a one-click allow takes effect quickly.", Label: "Block response TTL", Group: GroupBlocking, Presets: []string{"10s", "1m", "10m", "1h"}},
	{Path: "blocking.allowDegraded", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Report ready and serve while referenced blocklists are not loaded yet (queries are filtered by the lists that are loaded).", RestartRequired: true, Label: "Serve while lists load", Group: GroupBlocking},
	{Path: "blocking.customIP.v4", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv4 answer for blocked A queries when the response is customIP; empty answers NODATA.", Label: "Custom block IPv4", Group: GroupBlocking, Format: FormatIPv4},
	{Path: "blocking.customIP.v6", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv6 answer for blocked AAAA queries when the response is customIP; empty answers NODATA.", Label: "Custom block IPv6", Group: GroupBlocking, Format: FormatIPv6},
	{Path: "blockPage.enabled", Type: TypeBool, Scope: ScopeCluster, Default: false, Help: "Serve the block page on listeners.blockPage and allow the blockPage block response, which answers blocked A/AAAA queries with blockPage.ipv4/ipv6 (other types NODATA) so browsers show a page explaining the block.", RestartRequired: true, Label: "Block page enabled", Group: GroupBlockPage},
	{Path: "blockPage.ipv4", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv4 address blocked A queries are answered with under the blockPage response: this node's address, or a VIP or load balancer in front of the block page. Empty uses the first specific address of listeners.blockPage, else of listeners.dns; with neither, blockPage.enabled is refused.", Label: "Block page IPv4 address", Group: GroupBlockPage, Format: FormatIPv4},
	{Path: "blockPage.ipv6", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv6 address blocked AAAA queries are answered with under the blockPage response; empty derives it like blockPage.ipv4, and without one AAAA queries get NODATA.", Label: "Block page IPv6 address", Group: GroupBlockPage, Format: FormatIPv6},
	{Path: "blockPage.title", Type: TypeString, Scope: ScopeCluster, Default: "This site is blocked", Help: "Heading of the block page (plain text, at most 200 characters).", Label: "Page title", Group: GroupBlockPage},
	{Path: "blockPage.message", Type: TypeString, Scope: ScopeCluster, Default: "The administrator of this network has blocked access to this domain.", Help: "Explanation under the heading (plain text, at most 2000 characters; HTML is shown as text).", Label: "Page message", Group: GroupBlockPage},
	{Path: "blockPage.contact", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "Administrator contact shown as \"Ask your administrator\": an email address or an http(s) URL; empty omits the line.", Label: "Administrator contact", Group: GroupBlockPage},
	{Path: "blockPage.showDetails", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Show the client what blocked the name (list, rule, blocked service, safe search, policy), looked up in the recent query log; false shows only the domain, client, node and time.", Label: "Show block details", Group: GroupBlockPage},
	{Path: "blockPage.dashboardURL", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "Dashboard URL the block page links administrators to (<url>/queries?domain=...); empty derives it from the block page address and listeners.api.", Label: "Dashboard URL", Group: GroupBlockPage, Format: FormatURL},
	{Path: "blockPage.rateLimit", Type: TypeInt, Scope: ScopeCluster, Default: 120, Help: "Block page requests per minute per client address (bursts up to the same number); over the limit the listener answers 429. 0 disables the limit.", Label: "Requests per minute per client", Group: GroupBlockPage, Min: new(0), Max: new(100000)},
	{Path: "querylog.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Collect the per-node query log (querylog.db, rollups and the stats API).", RestartRequired: true, Label: "Query log enabled", Group: GroupQuerylog},
	{Path: "querylog.sink", Type: TypeString, Scope: ScopeCluster, Default: "sqlite", Help: "Built-in query-log sink: sqlite (querylog.db with rollups; required for the dashboard) or slog (one JSON log line per query, nothing stored).", RestartRequired: true, Label: "Query log sink", Group: GroupQuerylog, Enum: []Option{{Value: "sqlite", Label: "SQLite", Help: "querylog.db with rollups; required for the dashboard."}, {Value: "slog", Label: "Log lines", Help: "One JSON log line per query, nothing stored."}}},
	{Path: "querylog.privacy", Type: TypeString, Scope: ScopeCluster, Default: "full", Help: "Privacy applied once at ingest, irreversibly: full, anonymize_client, hide_domains, hide_domains_clients or nothing (per-minute totals only). The answer summary is stored only in full.", Label: "Query log privacy", Group: GroupQuerylog, Enum: []Option{{Value: "full", Label: "Full", Help: "Everything, including the answer summary."}, {Value: "anonymize_client", Label: "Anonymize clients", Help: "Client addresses hidden as set by querylog.anonymize."}, {Value: "hide_domains", Label: "Hide domains", Help: "Query names are not stored."}, {Value: "hide_domains_clients", Label: "Hide domains and clients", Help: "Neither query names nor clients are stored."}, {Value: "nothing", Label: "Totals only", Help: "Per-minute totals only, no rows."}}},
	{Path: "querylog.anonymize", Type: TypeString, Scope: ScopeCluster, Default: "prefix", Help: "How anonymize_client hides clients: prefix (IPv4 /24, IPv6 /48) or pseudonym (HMAC with a random in-memory key rotated daily; stable for a day, not reversible).", Label: "Client anonymization", Group: GroupQuerylog, Enum: []Option{{Value: "prefix", Label: "Prefix", Help: "IPv4 /24, IPv6 /48."}, {Value: "pseudonym", Label: "Pseudonym", Help: "HMAC with a random in-memory key rotated daily; stable for a day, not reversible."}}},
	{Path: "querylog.retention.raw", Type: TypeDuration, Scope: ScopeCluster, Default: "720h", Help: "How long per-query rows are kept (rollups: 35 d per minute, 400 d per hour, daily forever); 0 keeps rows until maxDBBytes is reached.", Label: "Keep query rows for", Group: GroupQuerylog, Presets: []string{"0s", "24h", "168h", "720h", "2160h"}},
	{Path: "querylog.retention.maxDBBytes", Type: TypeBytes, Scope: ScopeCluster, Default: "10GiB", Help: "Upper bound for querylog.db; the oldest rows are deleted first when it is exceeded. 0 disables the cap.", Label: "Maximum query log size", Group: GroupQuerylog, Presets: []string{"0", "1GiB", "5GiB", "10GiB", "50GiB"}},
	{Path: "querylog.exclude.domains", Type: TypeStringList, Scope: ScopeCluster, Default: []string{}, Help: "Names never logged: exact names, *.suffix wildcards (the suffix and everything below it) or /regex/.", Label: "Domains never logged", Group: GroupQuerylog, Items: &Shape{Type: TypeString}},
	{Path: "querylog.exclude.clients", Type: TypeStringList, Scope: ScopeCluster, Default: []string{}, Help: "Clients never logged: IP addresses, CIDR prefixes or client names.", Label: "Clients never logged", Group: GroupQuerylog, Items: &Shape{Type: TypeString}},
	{Path: "querylog.exclude.sudn", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Do not log special-use names (localhost, .local, .home.arpa, private and link-local reverse zones).", Label: "Skip special-use names", Group: GroupQuerylog},
	{Path: "blocking.refreshInterval", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "Refresh interval of BlockLists that set no spec.refreshInterval (at least 1m); the leader fetches, replicas pull by digest.", RestartRequired: true, Label: "List refresh interval", Group: GroupBlocking, Presets: []string{"1h", "6h", "12h", "24h", "168h"}},
	{Path: "blocking.initStrategy", Type: TypeString, Scope: ScopeCluster, Default: "fast", Help: "Blocklist loading at start: fast (serve at once from the disk cache, load in the background), blocking (not ready until every list is loaded), failOnError (blocking, and exit when a list fails to load).", RestartRequired: true, Label: "List loading at start", Group: GroupBlocking, Enum: []Option{{Value: "fast", Label: "Fast", Help: "Serve at once from the disk cache, load in the background."}, {Value: "blocking", Label: "Wait for lists", Help: "Not ready until every list is loaded."}, {Value: "failOnError", Label: "Fail on error", Help: "Wait for every list, and exit when one fails to load."}}},
	{Path: "blocking.listsDir", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Blocklist body cache (sha256(url) files); empty means <node.dataDir>/lists.", HostOnly: true, RestartRequired: true, Label: "List cache directory", Group: GroupBlocking},
	{Path: "blocking.allowLocalListURLs", Type: TypeBool, Scope: ScopeNode, Default: false, Help: "Let BlockList URLs reach loopback, link-local (169.254.0.0/16, fe80::/10: cloud metadata), unspecified and multicast addresses. Off by default so a BlockList writer cannot make the node fetch its own or the cloud's internal services (SSRF); private LAN ranges are always allowed.", HostOnly: true, RestartRequired: true, Label: "Allow local list URLs", Group: GroupBlocking},
	{Path: "blocking.catalogue.url", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "URL of a blocked-services and safe-search catalogue document (schema version 1, docs/deploy/blocked-services.md) that replaces the embedded one; empty serves the catalogue built into the binary. The leader downloads it, replicas pull the leader's copy; a document that fails validation keeps the catalogue in service.", RestartRequired: true, Label: "Catalogue URL", Group: GroupBlocking, Format: FormatURL},
	{Path: "blocking.catalogue.refreshInterval", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "How often the leader downloads blocking.catalogue.url (at least 1m; conditional GET).", RestartRequired: true, Label: "Catalogue refresh interval", Group: GroupBlocking, Presets: []string{"6h", "24h", "168h"}},
	{Path: "ratelimit.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Per-prefix QPM rate limiting of UDP queries at ingress; TCP, DoT, DoH, DoQ and DoH3 are never limited, nor loopback clients and the health name.", Label: "Rate limiting enabled", Group: GroupRateLimit},
	{Path: "ratelimit.ipv4", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"32": 600, "24": 6000}, Help: "IPv4 prefix length to queries per minute per prefix (token bucket holding one minute's allowance); merged with the defaults key by key, 0 disables a level.", Label: "IPv4 limits (queries per minute)", Group: GroupRateLimit, Keys: &Shape{Type: TypeInt, Min: new(0), Max: new(32)}, Values: rateLevelValue},
	{Path: "ratelimit.ipv6", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"128": 600, "64": 6000, "56": 12000}, Help: "IPv6 prefix length to queries per minute per prefix; merged with the defaults key by key, 0 disables a level.", Label: "IPv6 limits (queries per minute)", Group: GroupRateLimit, Keys: &Shape{Type: TypeInt, Min: new(0), Max: new(128)}, Values: rateLevelValue},
	{Path: "ratelimit.slip", Type: TypeInt, Scope: ScopeCluster, Default: 50, Help: "Percentage (0-100) of over-limit UDP queries answered with an empty TC=1 response so real clients retry over TCP; the rest are dropped.", Label: "Slip (percent answered TC=1)", Group: GroupRateLimit, Min: new(0), Max: new(100)},
	{Path: "ratelimit.maxBuckets", Type: TypeInt, Scope: ScopeCluster, Default: 65536, Help: "Most prefixes tracked at once (LRU); an evicted prefix starts with a full bucket.", Label: "Tracked prefixes", Group: GroupRateLimit, Min: new(0)},
	{Path: "ratelimit.exempt", Type: TypeStringList, Scope: ScopeCluster, Default: []string{}, Help: "CIDRs or addresses never rate limited (loopback always is).", Label: "Exempt clients", Group: GroupRateLimit, Items: &Shape{Type: TypeString, Format: FormatCIDR}},
	{Path: "objects", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Inline CRD-shaped objects (zones, lists, groups, policies); file-managed as file:kudurru.yaml and re-applied on start.", HostOnly: true, RestartRequired: true, Label: "Inline objects", Group: GroupNode, Items: objectShape},
	{Path: "settingsFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Settings file layered over this file for the cluster-scope keys and watched for changes (hot reload, D25); KUDURRU_SETTINGS_FILE takes precedence. The Helm chart mounts /etc/kudurru/settings/settings.yaml; in Docker PATCH /api/v1/settings persists into it. Empty disables the layer.", HostOnly: true, RestartRequired: true, Label: "Settings file", Group: GroupNode},
	{Path: "objectsDir", Type: TypeString, Scope: ScopeNode, Default: "/etc/kudurru/objects.d", Help: "Directory of *.yaml object files, file-managed as file:<dir>/<name> and re-applied on change; deleting a document deletes the object.", HostOnly: true, RestartRequired: true, Label: "Objects directory", Group: GroupNode},
	{Path: "tsigKeys", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "TSIG keys (RFC 8945) for zone transfers and NOTIFY: name (referenced by DNSZone spec.policy.transferTSIGKeyRefs), algorithm (hmac-sha256, hmac-sha512) and secretFile holding the base64 secret (tsig-keygen format).", HostOnly: true, RestartRequired: true, WriteOnly: true, Label: "TSIG keys", Group: GroupZones, Items: tsigKeyShape},
	{Path: "notify.source", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Local IP address outbound NOTIFY messages are sent from; empty lets the kernel choose. Secondaries must list it as a primary.", HostOnly: true, RestartRequired: true, Label: "NOTIFY source address", Group: GroupZones, Format: FormatIP},
	{Path: "externalDNS.webhook.enabled", Type: TypeBool, Scope: ScopeNode, Default: false, Help: "Serve the external-dns webhook provider (GET /, GET/POST /records, POST /adjustendpoints) on externalDNS.webhook.listen.", HostOnly: true, RestartRequired: true, Label: "external-dns webhook enabled", Group: GroupIntegrations},
	{Path: "externalDNS.webhook.listen", Type: TypeString, Scope: ScopeNode, Default: "127.0.0.1:8888", Help: "Listener of the external-dns webhook. It has no authentication, so the default is loopback (external-dns as a sidecar or on the same host); a wider address (the chart renders :<port> behind its NetworkPolicy) lets every client that reaches it write records.", HostOnly: true, RestartRequired: true, Label: "external-dns webhook address", Group: GroupIntegrations, Format: FormatHostPort},
	{Path: "externalDNS.webhook.namespace", Type: TypeString, Scope: ScopeNode, Default: "external-dns", Help: "Namespace of the DNSRecordSets the webhook writes (managed by integration:external-dns, one record set per DNS name).", HostOnly: true, RestartRequired: true, Label: "external-dns record namespace", Group: GroupIntegrations},
	{Path: "externalDNS.webhook.zoneSelector", Type: TypeMap, Scope: ScopeNode, Default: map[string]any{}, Help: "Label selector (key: value) choosing the primary DNSZones external-dns may write to; empty allows every primary zone.", HostOnly: true, RestartRequired: true, Label: "external-dns zone selector", Group: GroupIntegrations, Keys: &Shape{Type: TypeString}, Values: &Shape{Type: TypeString}},
	{Path: "auth.localLogin", Type: TypeString, Scope: ScopeCluster, Default: "enabled", Help: "Local password login (POST /api/v1/auth/login): enabled, admins-only (builtin admins only) or disabled; `kudurru admin` over the admin socket always works.", RestartRequired: true, Label: "Local password login", Group: GroupSecurity, Enum: []Option{{Value: "enabled", Label: "Enabled", Help: "Every local user may log in with a password."}, {Value: "admins-only", Label: "Admins only", Help: "Only builtin admins may log in with a password."}, {Value: "disabled", Label: "Disabled", Help: "No password login; kudurru admin over the admin socket always works."}}},
	{Path: "auth.bootstrap.adminPasswordFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "File holding the first-boot admin password; KUDURRU_ADMIN_PASSWORD[_FILE] take precedence, and without either a password is generated and printed once.", HostOnly: true, RestartRequired: true, Label: "First-boot admin password file", Group: GroupSecurity},
	{Path: "auth.session.idleTimeout", Type: TypeDuration, Scope: ScopeCluster, Default: "30m", Help: "Sliding idle timeout of a UI session (at least 1m).", RestartRequired: true, Label: "Session idle timeout", Group: GroupSecurity, Presets: []string{"15m", "30m", "1h", "8h"}},
	{Path: "auth.session.lifetime", Type: TypeDuration, Scope: ScopeCluster, Default: "12h", Help: "Absolute lifetime of a UI session; roles are re-resolved at the next login.", RestartRequired: true, Label: "Session lifetime", Group: GroupSecurity, Presets: []string{"8h", "12h", "24h", "168h"}},
	{Path: "auth.session.secure", Type: TypeString, Scope: ScopeCluster, Default: "auto", Help: "Session cookie Secure attribute: auto (when TLS terminates here or a trusted proxy reports https), always or never.", RestartRequired: true, Label: "Secure session cookie", Group: GroupSecurity, Enum: []Option{{Value: "auto", Label: "Auto", Help: "When TLS terminates here or a trusted proxy reports https."}, {Value: "always", Label: "Always", Help: "Behind a proxy that terminates TLS without telling kudurru."}, {Value: "never", Label: "Never", Help: "Plain HTTP only; the cookie can be read on the wire."}}},
	{Path: "auth.tokens.maxLifetime", Type: TypeDuration, Scope: ScopeCluster, Default: "0s", Help: "Longest lifetime of a new API token; tokens without expiry get it. 0 allows tokens that never expire.", RestartRequired: true, Label: "Maximum token lifetime", Group: GroupSecurity, Presets: []string{"0s", "720h", "2160h", "8760h"}},
	{Path: "auth.identityProviders", Type: TypeList, Scope: ScopeCluster, Default: []map[string]any{}, Help: "OpenID Providers for UI login and bearer ID tokens: id, issuer, clientID, clientSecretFile, redirectURL, cliClientID, scopes, claimValidationRules, claimMappings, provisioning.", HostOnly: true, RestartRequired: true, Label: "Identity providers", Group: GroupSecurity, Items: identityProviderShape},
	{Path: "auth.kubernetes.mode", Type: TypeString, Scope: ScopeCluster, Default: "sar", Help: "How Kubernetes service-account tokens are authorized: sar (SubjectAccessReview on kudurru.io resources). impersonate is v0.2.", HostOnly: true, RestartRequired: true, Label: "Kubernetes authorization", Group: GroupSecurity, Enum: []Option{{Value: "sar", Label: "SubjectAccessReview", Help: "Service-account tokens are authorized with a SubjectAccessReview on kudurru.io resources."}}, normalize: allowPlannedKubernetesMode},
	{Path: "auth.adminSocket.path", Type: TypeString, Scope: ScopeNode, Default: "/run/kudurru/kudurru.sock", Help: "Unix socket for `kudurru admin` (reset-password, create-token, list-users); peers must be uid 0, the server's uid or in adminSocket.group. Empty disables it.", HostOnly: true, RestartRequired: true, Label: "Admin socket path", Group: GroupSecurity},
	{Path: "auth.adminSocket.group", Type: TypeInt, Scope: ScopeNode, Default: -1, Help: "Extra gid allowed on the admin socket; -1 for none.", HostOnly: true, RestartRequired: true, Label: "Admin socket group (gid)", Group: GroupSecurity, Min: new(-1)},
	{Path: "rbac.bindings", Type: TypeList, Scope: ScopeCluster, Default: []map[string]any{}, Help: "Role bindings: role, scope {kind cluster|zone, name} and subjects [{kind User|Group|ServiceAccount, name, namespace}]; RoleBinding objects add to them.", HostOnly: true, RestartRequired: true, Label: "Role bindings", Group: GroupSecurity, Items: roleBindingShape},
	{Path: "log.level", Type: TypeString, Scope: ScopeNode, Default: "info", Help: "Log level: debug, info, warn, error. Per-role overrides as info,dns=debug.", Label: "Log level", Group: GroupLogging, Enum: []Option{{Value: "debug", Label: "Debug"}, {Value: "info", Label: "Info"}, {Value: "warn", Label: "Warning"}, {Value: "error", Label: "Error"}}, normalize: baseLogLevel},
	{Path: "log.format", Type: TypeString, Scope: ScopeNode, Default: "json", Help: "Log format: json or text.", RestartRequired: true, Label: "Log format", Group: GroupLogging, Enum: []Option{{Value: "json", Label: "JSON"}, {Value: "text", Label: "Text"}}},
}

// Lookup finds a key by path.
func Lookup(path string) (Key, bool) {
	for _, k := range Schema {
		if k.Path == path {
			return k, true
		}
	}
	return Key{}, false
}

// Paths returns all schema paths sorted.
func Paths() []string {
	out := make([]string, 0, len(Schema))
	for _, k := range Schema {
		out = append(out, k.Path)
	}
	sort.Strings(out)
	return out
}

// Defaults returns the flat default map used as the first koanf layer.
func Defaults() map[string]any {
	out := make(map[string]any, len(Schema))
	for _, k := range Schema {
		out[k.Path] = k.Default
	}
	return out
}

// lowerPaths maps lower-cased paths to canonical paths for env lookup.
func lowerPaths() map[string]string {
	out := make(map[string]string, len(Schema))
	for _, k := range Schema {
		out[strings.ToLower(k.Path)] = k.Path
	}
	return out
}

// baseLogLevel is the level before the per-role overrides
// ("info,dns=debug" → "info").
func baseLogLevel(v string) string { return strings.SplitN(v, ",", 2)[0] }

// allowPlannedKubernetesMode lets impersonate pass the enum test so
// Auth.validate can explain why it is not available yet (D55).
func allowPlannedKubernetesMode(v string) string {
	if v == "impersonate" {
		return "sar"
	}
	return v
}
