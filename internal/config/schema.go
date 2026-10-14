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
	// Help is the user-facing description the Settings page, `kudurru
	// config schema` and the settings reference show: a plain sentence on
	// what the key does first (with the unit and an example), then the
	// consequences of the common choices, then the technical detail.
	// TestSchemaHelpComplete requires it on every key, enum option and
	// nested field.
	Help string
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
	GroupDatabase     = "Central database"
	GroupRateLimit    = "Rate limiting"
	GroupSecurity     = "Security"
	GroupZones        = "Zone transfers"
	GroupIntegrations = "Integrations"
	GroupLogging      = "Logging"
)

// Groups lists the UI sections in display order.
var Groups = []string{
	GroupNode, GroupListeners, GroupUpstreams, GroupCache, GroupBlocking, GroupBlockPage, GroupQuerylog, GroupDatabase,
	GroupRateLimit, GroupSecurity, GroupZones, GroupIntegrations, GroupLogging,
}

// Closed sets shared by several keys.
var (
	// BlockResponses are the values of blocking.defaultResponse (the
	// BlockResponse values of api/v1alpha1).
	BlockResponses = []Option{
		{Value: "zeroIP", Label: "Zero IP", Help: "Answer 0.0.0.0 (A) or :: (AAAA), so the connection fails at once; other record types get an empty answer (NODATA). Suits most networks."},
		{Value: "nxdomain", Label: "NXDOMAIN", Help: "Answer that the name does not exist (NXDOMAIN), for every record type."},
		{Value: "customIP", Label: "Custom IP", Help: "Answer the addresses in blocking.customIP.v4 and v6, e.g. a server of yours; a family without an address gets NODATA."},
		{Value: "refused", Label: "REFUSED", Help: "Refuse the query. Discouraged: most devices then ask their next DNS server, which bypasses blocking."},
		{Value: "blockPage", Label: "Block page", Help: "Send browsers to the block page, which explains why the name was blocked (needs blockPage.enabled); other record types get NODATA."},
	}
	upstreamStrategies = []Option{
		{Value: "parallel_best", Label: "Parallel best", Help: "Ask two healthy servers picked at random at once and use the first good answer; if both fail, ask the rest. Fast and resilient; suits most setups."},
		{Value: "strict", Label: "Strict order", Help: "Ask the servers one at a time in the listed order, moving to the next only when one fails. Use it when the first server should answer whenever it can."},
		{Value: "random", Label: "Random", Help: "Ask one healthy server picked at random, and another random one if it fails. Spreads the load evenly."},
		{Value: "rtt_weighted", Label: "Fastest (RTT weighted)", Help: "Prefer the servers that answer fastest: the first is picked at random weighted by its inverse round-trip time, then the rest are tried in RTT order."},
	}
	dnsProtocols = []Option{
		{Value: "udp", Label: "UDP", Help: "Plain DNS over UDP; what most devices use."},
		{Value: "tcp", Label: "TCP", Help: "Plain DNS over TCP; used for large answers and by clients that retry over TCP."},
	}
	tsigAlgorithms = []Option{
		{Value: "hmac-sha256", Label: "HMAC-SHA256", Help: "The common choice; every current DNS server supports it."},
		{Value: "hmac-sha512", Label: "HMAC-SHA512", Help: "A longer digest; use it when the other server requires it."},
	}
	// rbacRoles are the roles of internal/rbac/permissions.yaml
	// (TestRBACRolesMatchPermissions).
	rbacRoles = []Option{
		{Value: "viewer", Label: "Viewer", Help: "Read zones, records, lists, policies, clients, stats and settings; change nothing."},
		{Value: "auditor", Label: "Auditor", Help: "Viewer plus reading the query log, the audit log and unblock requests."},
		{Value: "operator", Label: "Operator", Help: "Day-to-day blocking work: manage records, lists, policies, client groups, clients and unblock requests, toggle blocking and flush the cache. No zones, users or settings."},
		{Value: "zone-admin", Label: "Zone admin", Help: "Manage zones and their records, transfers and signing; bind it to a zone scope."},
		{Value: "admin", Label: "Admin", Help: "Everything, including users, role bindings and settings."},
	}
	bindingScopes = []Option{
		{Value: "cluster", Label: "Cluster", Help: "The role applies everywhere."},
		{Value: "zone", Label: "Zone", Help: "The role applies to the zone named in scope.name."},
		{Value: "zonegroup", Label: "Zone group", Help: "The role applies to the zone group named in scope.name (reserved: zone groups arrive in v0.2)."},
		{Value: "clientgroup", Label: "Client group", Help: "The role applies to the client group named in scope.name (reserved for v1.0)."},
	}
	subjectKinds = []Option{
		{Value: "User", Label: "User", Help: "One account by username: a local user (e.g. alice) or an identity-provider login (e.g. corp:alice)."},
		{Value: "Group", Label: "Group", Help: "Every member of a group, e.g. a local group or an identity-provider group such as corp:/dns-admins."},
		{Value: "ServiceAccount", Label: "Service account", Help: "A kudurru or Kubernetes service account, by name and namespace."},
	}
)

// listenerAddress is the address field of a listener entry; port is the
// usual port of its protocol.
func listenerAddress(port string) Field {
	return Field{
		Name: "address", Label: "Address",
		Help:     "Address and port to listen on: 0.0.0.0:" + port + ", [::]:" + port + " or :" + port + " for every address, or one address such as 192.168.1.10:" + port + ".",
		Required: true, Shape: Shape{Type: TypeString, Format: FormatHostPort},
	}
}

// Shapes of the list and map keys.
var (
	tlsFilesShape = Shape{Type: TypeObject, Fields: []Field{
		{Name: "certFile", Label: "Certificate file", Help: "Path of the certificate (PEM, full chain), e.g. /etc/kudurru/tls/tls.crt; it must be valid for the name clients use. Reloaded when the file changes.", Required: true, Shape: Shape{Type: TypeString}},
		{Name: "keyFile", Label: "Key file", Help: "Path of the private key (PEM) of certFile, e.g. /etc/kudurru/tls/tls.key.", Required: true, Shape: Shape{Type: TypeString}},
	}}
	listenerTLS      = Field{Name: "tls", Label: "TLS", Help: "Certificate and private key files of this listener.", Required: true, Shape: tlsFilesShape}
	listenerPath     = Field{Name: "path", Label: "Path", Help: "URL path of the endpoint, starting with /; empty means /dns-query, what most clients expect.", Shape: Shape{Type: TypeString, Default: "/dns-query"}}
	dnsListenerShape = &Shape{Type: TypeObject, Fields: []Field{
		listenerAddress("53"),
		{Name: "protocols", Label: "Protocols", Help: "Transports served on this address: udp, tcp or both. Empty serves both, which is what devices expect.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Enum: dnsProtocols}, Default: []string{"udp", "tcp"}}},
	}}
	dotListenerShape   = &Shape{Type: TypeObject, Fields: []Field{listenerAddress("853"), listenerTLS}}
	httpListenerShape  = &Shape{Type: TypeObject, Fields: []Field{listenerAddress("443"), listenerPath, listenerTLS}}
	upstreamGroupShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "servers", Label: "Servers", Help: "DNS servers this group forwards to, e.g. 1.1.1.1, tls://dns.quad9.net or https://dns.google/dns-query. A bare address uses plain DNS on port 53; tls://, https://, quic://, h3:// and sdns:// stamps encrypt the queries. Prefix a display name with name=, e.g. cloudflare=tls://1.1.1.1.", Required: true,
			Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatUpstream}}},
		{Name: "strategy", Label: "Strategy", Help: "How the servers are picked for each query; parallel_best suits most setups.", Shape: Shape{Type: TypeString, Enum: upstreamStrategies, Default: "parallel_best"}},
		{Name: "timeout", Label: "Timeout", Help: "How long to wait for one server's answer before trying another, e.g. 2s; raise it for slow or distant servers.", Shape: Shape{Type: TypeDuration, Presets: []string{"500ms", "1s", "2s", "5s"}, Default: "2s"}},
	}}
	tsigKeyShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "name", Label: "Name", Help: "Name of the key, a domain name such as xfr.home.example; the other server must use the same name, and zones list it in spec.policy.transferTSIGKeyRefs.", Required: true, Shape: Shape{Type: TypeString, Format: FormatDomain}},
		{Name: "algorithm", Label: "Algorithm", Help: "HMAC algorithm of the key; it must match the other server.", Required: true, Shape: Shape{Type: TypeString, Enum: tsigAlgorithms, normalize: func(v string) string {
			return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
		}}},
		{Name: "secretFile", Label: "Secret file", Help: "Path of a file holding only the base64 secret, e.g. /etc/kudurru/xfr.key: the quoted value of the secret line tsig-keygen prints, or the output of openssl rand -base64 32.", Required: true, Shape: Shape{Type: TypeString}},
	}}
	identityProviderShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "id", Label: "ID", Help: "Short name of the provider, a lowercase DNS label such as corp. It appears in the login URLs and, as corp:, in front of every username and group from this provider.", Required: true, Shape: Shape{Type: TypeString}},
		{Name: "displayName", Label: "Display name", Help: "Name on the login button, e.g. Company SSO; empty shows the id.", Shape: Shape{Type: TypeString}},
		{Name: "issuer", Label: "Issuer", Help: "Issuer URL of the OpenID Provider exactly as it publishes it, e.g. https://accounts.google.com or https://sso.home.example/realms/home; https is required except on loopback.", Shape: Shape{Type: TypeString, Format: FormatURL}},
		{Name: "clientID", Label: "Client ID", Help: "Client ID of the web application registered for kudurru at the provider.", Shape: Shape{Type: TypeString}},
		{Name: "clientSecretFile", Label: "Client secret file", Help: "Path of a file holding the client secret of clientID, e.g. /etc/kudurru/oidc-secret; required, because kudurru signs in as a confidential client.", Shape: Shape{Type: TypeString}},
		{Name: "cliClientID", Label: "CLI client ID", Help: "Public client ID for signing in from the command line (kudurru login, v0.2); ID tokens issued to it are accepted as bearer tokens. Empty for none.", Shape: Shape{Type: TypeString}},
		{Name: "redirectURL", Label: "Redirect URL", Help: "Callback URL registered at the provider: the dashboard URL followed by /api/v1/auth/oidc/<id>/callback, e.g. https://dns.home.example/api/v1/auth/oidc/corp/callback.", Shape: Shape{Type: TypeString, Format: FormatURL}},
		{Name: "scopes", Label: "Scopes", Help: "Scopes requested at sign-in; empty requests openid, profile, email and groups. openid is always added.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString}}},
		{Name: "audiences", Label: "Audiences", Help: "Extra audience (aud) values accepted on bearer ID tokens from this provider; empty for none.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString}}},
		{Name: "claimValidationRules", Label: "Claim validation rules", Help: "Claims every sign-in must carry, as {claim, requiredValue} pairs, e.g. claim email_verified with requiredValue \"true\"; a sign-in that fails one is refused.", Shape: Shape{Type: TypeList, Items: &Shape{Type: TypeObject}}},
		{Name: "claimMappings", Label: "Claim mappings", Help: "Which claims give the username, groups, display name and email: {username: {claim, prefix}, groups: {claim, prefix}, displayName: {claim}, email: {claim}}. Empty uses preferred_username (then upn, a verified email, sub), groups and roles, name and email; prefixes default to <id>:.", Shape: Shape{Type: TypeObject}},
		{Name: "provisioning", Label: "Provisioning", Help: "Who may sign in the first time, as {autoProvision, defaultRole}: mapped-only (default) admits only sign-ins a role binding matches; always admits everyone the provider vouches for; never admits only identities kudurru already knows. defaultRole (not admin) is granted to an admitted sign-in without a binding.", Shape: Shape{Type: TypeObject}},
		{Name: "allowedDomains", Label: "Allowed email domains", Help: "Reserved: email domains sign-ins will be restricted to, e.g. home.example. Accepted but not enforced yet.", Shape: Shape{Type: TypeStringList, Items: &Shape{Type: TypeString, Format: FormatDomain}}},
	}}
	roleBindingShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "role", Label: "Role", Help: "Role granted to the subjects.", Required: true, Shape: Shape{Type: TypeString, Enum: rbacRoles}},
		{Name: "scope", Label: "Scope", Help: "Where the role applies; empty means the whole cluster.", Shape: Shape{Type: TypeObject, Fields: []Field{
			{Name: "kind", Label: "Kind", Help: "What the role is limited to: the whole cluster, one zone, a zone group or a client group.", Shape: Shape{Type: TypeString, Enum: bindingScopes, Default: "cluster"}},
			{Name: "name", Label: "Name", Help: "Zone, zone group or client group name, e.g. home.example; empty for cluster scope.", Shape: Shape{Type: TypeString}},
		}}},
		{Name: "subjects", Label: "Subjects", Help: "Who gets the role: users, groups or service accounts; at least one.", Required: true, Shape: Shape{Type: TypeList, Items: &Shape{Type: TypeObject, Fields: []Field{
			{Name: "kind", Label: "Kind", Help: "Whether name is a user, a group or a service account.", Required: true, Shape: Shape{Type: TypeString, Enum: subjectKinds}},
			{Name: "name", Label: "Name", Help: "Username, group or service-account name, e.g. alice, corp:alice or corp:/dns-admins; names from an identity provider carry its id as a prefix.", Required: true, Shape: Shape{Type: TypeString}},
			{Name: "namespace", Label: "Namespace", Help: "Namespace of the service account, e.g. kudurru; required for a ServiceAccount, empty for users and groups.", Shape: Shape{Type: TypeString}},
		}}}},
	}}
	objectShape = &Shape{Type: TypeObject, Fields: []Field{
		{Name: "apiVersion", Label: "API version", Help: "API version of the object, e.g. kudurru.io/v1alpha1.", Shape: Shape{Type: TypeString}},
		{Name: "kind", Label: "Kind", Help: "Kind of the object, e.g. DNSZone, DNSRecordSet, BlockList, ClientGroup or Policy.", Shape: Shape{Type: TypeString}},
		{Name: "metadata", Label: "Metadata", Help: "Object metadata: the name, and optionally labels, as in a Kubernetes manifest.", Shape: Shape{Type: TypeObject}},
		{Name: "spec", Label: "Spec", Help: "The object's settings, as described for its kind in the CRD reference.", Shape: Shape{Type: TypeObject}},
	}}
	rateLevelValue = &Shape{Type: TypeInt, Min: new(0)}
)

// Schema is the settings metadata table. Every leaf of Config must have an
// entry; TestSchemaMatchesConfig enforces it.
var Schema = []Key{
	{Path: "node.name", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Name of this server in the dashboard, the query log and the block page, e.g. dns1. Empty uses the hostname. Every node of a cluster needs its own name; it identifies the node to the others.", HostOnly: true, RestartRequired: true, Label: "Node name", Group: GroupNode},
	{Path: "node.lameduck", Type: TypeDuration, Scope: ScopeNode, Default: "2s", Help: "How long the server keeps answering after it is told to stop, so devices and load balancers move to another server first, e.g. 2s; 0 stops at once. During this window readiness (/healthz/ready) answers 503 and the health name SERVFAIL, then the listeners close. The Helm chart also runs kudurru lameduck in preStop.", HostOnly: true, RestartRequired: true, Label: "Lameduck drain window", Group: GroupNode, Presets: []string{"0s", "2s", "5s", "10s"}},
	{Path: "node.dataDir", Type: TypeString, Scope: ScopeNode, Default: "/var/lib/kudurru", Help: "Directory where kudurru keeps its data, e.g. /var/lib/kudurru; it must be writable and should survive restarts and be backed up. It holds the object store (state.db), the query log (querylog.db), the zone snapshot (local.db), downloaded lists and keys.", HostOnly: true, RestartRequired: true, Label: "Data directory", Group: GroupNode},
	{Path: "listeners.dns", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{{"address": "0.0.0.0:53", "protocols": []string{"udp", "tcp"}}}, Help: "Addresses where devices send plain DNS queries. Port 53 is what devices expect; the default 0.0.0.0:53 answers on every address over UDP and TCP. Bind one address, e.g. 192.168.1.10:53, when another service on the machine already uses port 53.", HostOnly: true, RestartRequired: true, Label: "DNS listeners", Group: GroupListeners, Items: dnsListenerShape},
	{Path: "listeners.dot", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Encrypted DNS-over-TLS (DoT), usually on port 853, for devices that support it such as Android's Private DNS. Each entry needs an address, e.g. 0.0.0.0:853, and a certificate and key valid for the name devices use. Empty serves none. RFC 7858, ALPN dot.", HostOnly: true, RestartRequired: true, Label: "DNS-over-TLS listeners", Group: GroupListeners, Items: dotListenerShape},
	{Path: "listeners.doh", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Encrypted DNS-over-HTTPS (DoH), usually on port 443, for browsers and devices that support it. Each entry needs an address, e.g. 0.0.0.0:443, a certificate and key, and optionally a path (default /dns-query). Empty serves none. RFC 8484, HTTP/2 and HTTP/1.1.", HostOnly: true, RestartRequired: true, Label: "DNS-over-HTTPS listeners", Group: GroupListeners, Items: httpListenerShape},
	{Path: "listeners.doq", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Encrypted DNS-over-QUIC (DoQ), usually on UDP port 853, for clients that support it. Each entry needs an address, e.g. 0.0.0.0:853, and a certificate and key. Empty serves none. RFC 9250, ALPN doq.", HostOnly: true, RestartRequired: true, Label: "DNS-over-QUIC listeners", Group: GroupListeners, Items: dotListenerShape},
	{Path: "listeners.doh3", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Encrypted DNS-over-HTTPS over HTTP/3 (QUIC), usually on UDP port 443, for browsers that support it. Each entry needs an address, e.g. 0.0.0.0:443, a certificate and key, and optionally a path (default /dns-query). Empty serves none. ALPN h3.", HostOnly: true, RestartRequired: true, Label: "DNS-over-HTTP/3 listeners", Group: GroupListeners, Items: httpListenerShape},
	{Path: "listeners.api.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8080", Help: "Address and port of the dashboard and the HTTP API, e.g. 0.0.0.0:8080 for every address or 127.0.0.1:8080 for this machine only; browse to https://<server address>:8080.", HostOnly: true, RestartRequired: true, Label: "API and UI address", Group: GroupListeners, Format: FormatHostPort, Presets: []string{"0.0.0.0:8080", "127.0.0.1:8080", ":443"}},
	{Path: "listeners.api.tls.auto", Type: TypeString, Scope: ScopeNode, Default: "selfsigned", Help: "Whether the dashboard uses HTTPS with a certificate kudurru makes itself when no certificate file is set. selfsigned (default) encrypts the connection, though browsers warn because no public authority signed the certificate; off serves plain HTTP, for use behind a reverse proxy that handles TLS. The certificate is generated once into <node.dataDir>/keys/.", HostOnly: true, RestartRequired: true, Label: "API automatic TLS", Group: GroupListeners, Enum: []Option{{Value: "selfsigned", Label: "Self-signed", Help: "HTTPS with a certificate generated once into <node.dataDir>/keys/; browsers warn until you trust it."}, {Value: "off", Label: "Off (plain HTTP)", Help: "Plain HTTP, only behind a reverse proxy that terminates TLS: otherwise passwords cross the network unencrypted."}}},
	{Path: "listeners.api.tls.certFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of your own certificate for the dashboard (PEM, full chain), e.g. /etc/kudurru/tls/tls.crt; set it with the key file to replace the self-signed certificate. Takes precedence over listeners.api.tls.auto.", HostOnly: true, RestartRequired: true, Label: "API certificate file", Group: GroupListeners},
	{Path: "listeners.api.tls.keyFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of the private key (PEM) of listeners.api.tls.certFile, e.g. /etc/kudurru/tls/tls.key.", HostOnly: true, RestartRequired: true, Label: "API key file", Group: GroupListeners},
	{Path: "listeners.api.pathPrefix", Type: TypeString, Scope: ScopeNode, Default: "", Help: "URL path the dashboard and API are served under when a reverse proxy mounts kudurru below the root of a site, e.g. /kudurru for https://home.example/kudurru/. Empty serves them at /.", HostOnly: true, RestartRequired: true, Label: "API path prefix", Group: GroupListeners},
	{Path: "listeners.api.trustedProxies", Type: TypeStringList, Scope: ScopeNode, Default: []string{}, Help: "Reverse proxies in front of the dashboard, as addresses or CIDRs, e.g. 192.168.1.5 or 10.0.0.0/8. Only requests from them may report the real client address and scheme in X-Forwarded-For and X-Forwarded-Proto; empty trusts no proxy.", HostOnly: true, RestartRequired: true, Label: "Trusted reverse proxies", Group: GroupListeners, Items: &Shape{Type: TypeString, Format: FormatCIDR}},
	{Path: "listeners.ops.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8081", Help: "Address and port of the health checks Docker, Kubernetes and load balancers poll, e.g. 0.0.0.0:8081 (/healthz/live, /healthz/ready). They need no login.", HostOnly: true, RestartRequired: true, Label: "Health listener address", Group: GroupListeners, Format: FormatHostPort, Presets: []string{"0.0.0.0:8081", "127.0.0.1:8081"}},
	{Path: "listeners.metrics.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:9153", Help: "Address and port Prometheus scrapes metrics from (/metrics), e.g. 0.0.0.0:9153, or 127.0.0.1:9153 to keep them on this machine.", HostOnly: true, RestartRequired: true, Label: "Metrics listener address", Group: GroupListeners, Format: FormatHostPort, Presets: []string{"0.0.0.0:9153", "127.0.0.1:9153"}},
	{Path: "listeners.proxyProtocolFrom", Type: TypeStringList, Scope: ScopeNode, Default: []string{}, Help: "Only with a dnsdist front end: the peers allowed to tell kudurru the real client address in a PROXY protocol v2 header, as addresses or CIDRs, e.g. 192.168.1.2/32. Wider ranges, such as a pod CIDR, require listeners.proxyProtocolSecretFile. Leave empty when devices query kudurru directly. Use loopback only when dnsdist shares kudurru's network namespace.", HostOnly: true, RestartRequired: true, Label: "PROXYv2 peers", Group: GroupListeners, Items: &Shape{Type: TypeString, Format: FormatCIDR}},
	{Path: "listeners.proxyProtocolSecretFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of a file holding the secret shared with the dnsdist front end, at least 32 bytes, e.g. made with openssl rand -hex 32. Required when proxyProtocolFrom names a peer that is not loopback, so another host cannot forge client addresses. Carried in PROXYv2 TLV 0xE0.", HostOnly: true, RestartRequired: true, Label: "PROXYv2 secret file", Group: GroupListeners},
	{Path: "listeners.internal.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8082", Help: "Address and port the other nodes of a cluster use to reach this one, e.g. 0.0.0.0:8082; a single server does not need it reachable. Always TLS with a bearer token; it serves list bodies by digest and /internal/status.", HostOnly: true, RestartRequired: true, Label: "Node-to-node listener address", Group: GroupListeners, Format: FormatHostPort, Presets: []string{"0.0.0.0:8082"}},
	{Path: "listeners.internal.tokenFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of a file holding the token nodes present to each other. Empty (the usual choice) derives it from the encryption key, so every node with the same KUDURRU_ENCRYPTION_KEY agrees on it (HKDF).", HostOnly: true, RestartRequired: true, Label: "Internal token file", Group: GroupListeners},
	{Path: "listeners.internal.tls.certFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of the certificate (PEM) of the node-to-node listener, for your own PKI. Empty (the usual choice) issues one from a CA derived from the encryption key, which every node with the same key trusts.", HostOnly: true, RestartRequired: true, Label: "Internal certificate file", Group: GroupListeners},
	{Path: "listeners.internal.tls.keyFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of the private key (PEM) of listeners.internal.tls.certFile.", HostOnly: true, RestartRequired: true, Label: "Internal key file", Group: GroupListeners},
	{Path: "listeners.internal.tls.caFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of the CA bundle (PEM) trusted when this node connects to other nodes; set it together with your own certificates. Empty trusts the CA derived from the encryption key.", HostOnly: true, RestartRequired: true, Label: "Internal CA bundle", Group: GroupListeners},
	{Path: "listeners.internal.tls.serverName", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Name checked in other nodes' certificates, e.g. kudurru-internal.home.example; it must be a SAN of listeners.internal.tls.certFile. Empty uses the fixed name of the derived certificates.", HostOnly: true, RestartRequired: true, Label: "Internal TLS server name", Group: GroupListeners, Format: FormatDomain},
	{Path: "listeners.blockPage.http", Type: TypeString, Scope: ScopeNode, Default: ":80", Help: "Address and port where the block page answers browsers over plain HTTP, e.g. :80; empty turns it off. Under the blockPage block response, blocked names point at this server, so a browser that opens one lands here. Needs blockPage.enabled.", HostOnly: true, RestartRequired: true, Label: "Block page HTTP address", Group: GroupBlockPage, Format: FormatHostPort, Presets: []string{":80", "0.0.0.0:80", "[::]:80"}},
	{Path: "listeners.blockPage.https", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Optional address and port of the block page over HTTPS, e.g. :443; empty (the default) leaves it off. No certificate can be valid for the blocked names, so browsers show a certificate warning before the page, and sites that use HSTS show no page at all (docs/deploy/blocking-page.md).", HostOnly: true, RestartRequired: true, Label: "Block page HTTPS address", Group: GroupBlockPage, Format: FormatHostPort, Presets: []string{":443"}},
	{Path: "listeners.blockPage.tls.certFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of the certificate (PEM) of the HTTPS block page. Empty generates a self-signed one into <node.dataDir>/keys/blockpage.crt; a certificate of your own cannot match the blocked names either.", HostOnly: true, RestartRequired: true, Label: "Block page certificate file", Group: GroupBlockPage},
	{Path: "listeners.blockPage.tls.keyFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of the private key (PEM) of listeners.blockPage.tls.certFile.", HostOnly: true, RestartRequired: true, Label: "Block page key file", Group: GroupBlockPage},
	{Path: "listeners.sockets", Type: TypeInt, Scope: ScopeNode, Default: 0, Help: "How many sockets each UDP and TCP DNS listener opens, so several CPU cores read queries in parallel. 0 (default) opens one per CPU (GOMAXPROCS), 1 a single socket. Uses SO_REUSEPORT on Linux, each socket with its own read loop; other systems always use one.", HostOnly: true, RestartRequired: true, Label: "Sockets per DNS listener", Group: GroupListeners, Min: new(0)},
	{Path: "store.driver", Type: TypeString, Scope: ScopeNode, Default: "sqlite", Help: "Where kudurru keeps its objects (zones, lists, policies, users): sqlite, a database file under node.dataDir, for Docker and single servers; kube, Kubernetes custom resources, which the Helm chart sets.", HostOnly: true, RestartRequired: true, Label: "Store driver", Group: GroupNode, Enum: []Option{{Value: "sqlite", Label: "SQLite", Help: "A database file (state.db) under node.dataDir; for Docker and single servers."}, {Value: "kube", Label: "Kubernetes", Help: "Kubernetes custom resources (CRDs) through the apiserver; what the Helm chart uses."}}},
	{Path: "store.kube.startupTimeout", Type: TypeDuration, Scope: ScopeNode, Default: "15s", Help: "Kubernetes only: how long to wait at start for the apiserver to deliver the objects, e.g. 15s. After that the node answers from its last saved copy (local.db) until the apiserver catches up.", HostOnly: true, RestartRequired: true, Label: "Kubernetes sync timeout", Group: GroupNode, Presets: []string{"5s", "15s", "30s", "1m"}},
	{Path: "store.kube.snapshot.maxAge", Type: TypeDuration, Scope: ScopeNode, Default: "24h", Help: "Kubernetes only: the oldest saved copy (local.db) the node may answer its own zones from while the apiserver is unreachable, e.g. 24h. An older copy is not trusted: the node reports not ready and its primary zones answer SERVFAIL.", HostOnly: true, RestartRequired: true, Label: "Snapshot maximum age", Group: GroupNode, Presets: []string{"1h", "24h", "168h"}},
	{Path: "upstreams.groups", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"default": map[string]any{"servers": []string{"1.1.1.1:53", "9.9.9.9:53"}, "strategy": "parallel_best", "timeout": "2s"}}, Help: "The DNS servers kudurru asks for names it does not answer itself, in named groups; the group named default gets every query nothing else routes. Each group lists servers (e.g. 1.1.1.1, tls://dns.quad9.net), a strategy that picks among them and a timeout per server. Conditional forwarding, forward zones and policies send queries to other groups by name.", Label: "Upstream groups", Group: GroupUpstreams, Keys: &Shape{Type: TypeString}, Values: upstreamGroupShape},
	{Path: "upstreams.bootstrap", Type: TypeStringList, Scope: ScopeCluster, Default: []string{"1.1.1.1:53"}, Help: "Plain DNS servers used only to look up the host names of upstream servers, such as dns.quad9.net in tls://dns.quad9.net, e.g. 1.1.1.1:53 or 9.9.9.9. Never asked for client queries.", Label: "Bootstrap resolvers", Group: GroupUpstreams, Items: &Shape{Type: TypeString, Format: FormatUpstream}},
	{Path: "upstreams.conditional", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{}, Help: "Send queries for some domains to a chosen upstream group, e.g. home.lan to a group holding your router, so local device names resolve. Each entry maps a domain suffix to a group name; the longest matching suffix wins, and a key may list several suffixes in dnsmasq form, \"[/corp.example/10.in-addr.arpa/]\". These answers are never cached.", Label: "Conditional forwarding", Group: GroupUpstreams, Keys: &Shape{Type: TypeString}, Values: &Shape{Type: TypeString, Ref: RefUpstreamGroup}},
	{Path: "upstreams.ecs.mode", Type: TypeString, Scope: ScopeCluster, Default: "strip", Help: "Whether upstream servers learn which network a query came from (EDNS Client Subnet). strip (default) never tells them, the private choice; forward and add can get answers from servers nearer the client, at the cost of revealing part of its address.", Label: "EDNS Client Subnet", Group: GroupUpstreams, Enum: []Option{{Value: "strip", Label: "Strip", Help: "Never send a client subnet upstream; the private choice and the default."}, {Value: "forward", Label: "Forward", Help: "Pass on a subnet the client sent itself, shortened to the prefix lengths below."}, {Value: "add", Label: "Add", Help: "Like Forward, and add a subnet for clients with a public address that sent none."}}},
	{Path: "upstreams.ecs.ipv4Prefix", Type: TypeInt, Scope: ScopeCluster, Default: 24, Help: "How much of an IPv4 client address EDNS Client Subnet may reveal, as a prefix length from 0 to 32: with 24 (default) only the network, e.g. 203.0.113.0/24, is sent. Used only when upstreams.ecs.mode is forward or add.", Label: "ECS IPv4 prefix length", Group: GroupUpstreams, Min: new(0), Max: new(32)},
	{Path: "upstreams.ecs.ipv6Prefix", Type: TypeInt, Scope: ScopeCluster, Default: 56, Help: "How much of an IPv6 client address EDNS Client Subnet may reveal, as a prefix length from 0 to 128: with 56 (default) only the /56 network is sent. Used only when upstreams.ecs.mode is forward or add.", Label: "ECS IPv6 prefix length", Group: GroupUpstreams, Min: new(0), Max: new(128)},
	{Path: "cache.maxEntries", Type: TypeInt, Scope: ScopeCluster, Default: 200000, Help: "How many answers the cache holds, e.g. 200000; when it is full the least recently used answer is dropped. More entries mean more cache hits and more memory; 0 turns the cache off.", Label: "Maximum cache entries", Group: GroupCache, Min: new(0)},
	{Path: "cache.minTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "5s", Help: "Shortest time an answer stays cached, even when its record asks for less, e.g. 5s. Raising it saves upstream queries but keeps changed addresses longer; 0 honors every record's TTL.", Label: "Minimum TTL", Group: GroupCache, Presets: []string{"0s", "5s", "30s", "1m", "5m"}},
	{Path: "cache.maxTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "Longest time an answer stays cached, even when its record asks for more, e.g. 24h; lower it so changed addresses show up sooner. 0 sets no upper limit.", Label: "Maximum TTL", Group: GroupCache, Presets: []string{"1h", "6h", "24h", "168h"}},
	{Path: "cache.negativeTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "5m", Help: "Longest time a \"does not exist\" answer (NXDOMAIN or NODATA) stays cached, e.g. 5m; shorter lets a newly created name resolve sooner. Below it the zone's SOA minimum applies (RFC 2308).", Label: "Maximum negative TTL", Group: GroupCache, Presets: []string{"30s", "1m", "5m", "1h"}},
	{Path: "cache.prefetch.threshold", Type: TypeInt, Scope: ScopeCluster, Default: 5, Help: "Refresh popular answers shortly before they expire, so nobody waits for the upstream: an answer asked for this many times within cache.prefetch.window counts as popular, e.g. 5. 0 turns prefetching off.", Label: "Prefetch threshold (hits)", Group: GroupCache, Min: new(0)},
	{Path: "cache.prefetch.window", Type: TypeDuration, Scope: ScopeCluster, Default: "2h", Help: "Time over which the hits for cache.prefetch.threshold are counted, e.g. 2h.", Label: "Prefetch window", Group: GroupCache, Presets: []string{"30m", "1h", "2h", "6h"}},
	{Path: "cache.serveStale.ttl", Type: TypeDuration, Scope: ScopeCluster, Default: "72h", Help: "When every upstream server fails, keep answering from expired cache entries for up to this long after they expired, e.g. 72h, so names you already use keep working through an outage. 0 turns it off (RFC 8767 serve-stale).", Label: "Serve stale for", Group: GroupCache, Presets: []string{"0s", "1h", "24h", "72h"}},
	{Path: "cache.serveStale.answerTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "30s", Help: "TTL given to an expired answer served this way, e.g. 30s, so clients ask again soon; after a failed refresh, queries for the entry also stop waiting for the upstream for this long. 0 means 30s (RFC 8767).", Label: "Stale answer TTL", Group: GroupCache, Presets: []string{"5s", "30s", "1m"}},
	{Path: "cache.serveStale.maxWait", Type: TypeDuration, Scope: ScopeCluster, Default: "1800ms", Help: "How long a query for an expired entry waits for a fresh upstream answer before the expired one is sent, e.g. 1800ms; 0 means 1800ms (RFC 8767 client response timer).", Label: "Stale answer wait", Group: GroupCache, Presets: []string{"500ms", "1s", "1800ms", "3s"}},
	{Path: "blocking.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Block ads, trackers and other listed names for the whole network; off answers every query unfiltered. To pause blocking for a while, use the temporary disable on the dashboard instead (a runtime toggle, not this key). Applies to forwarded queries, not to your own zones.", Label: "Blocking enabled", Group: GroupBlocking},
	{Path: "blocking.defaultResponse", Type: TypeString, Scope: ScopeCluster, Default: "zeroIP", Help: "What a blocked query is answered with, unless the client's Policy (blockMode) or the BlockList (response) chooses otherwise. zeroIP (default) makes the connection fail at once and suits most networks; blockPage shows browsers a page that explains the block; refused is discouraged, because devices then ask another DNS server.", Label: "Default block response", Group: GroupBlocking, Enum: BlockResponses},
	{Path: "blocking.blockTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "10m", Help: "How long devices may remember a block answer, e.g. 10m, unless the Policy sets blockTTL. Keep it short: after you allow a name, devices keep the cached block until it expires.", Label: "Block response TTL", Group: GroupBlocking, Presets: []string{"10s", "1m", "10m", "1h"}},
	{Path: "blocking.allowDegraded", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Answer queries and report ready while blocklists are still loading, e.g. right after a restart; queries are filtered by the lists already loaded. Off, the node reports not ready until every referenced list is loaded, so load balancers keep traffic away.", RestartRequired: true, Label: "Serve while lists load", Group: GroupBlocking},
	{Path: "blocking.customIP.v4", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv4 address blocked A queries are answered with when the block response is customIP, e.g. 192.168.1.10 for a server of yours that shows a notice. Empty answers NODATA.", Label: "Custom block IPv4", Group: GroupBlocking, Format: FormatIPv4},
	{Path: "blocking.customIP.v6", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv6 address blocked AAAA queries are answered with when the block response is customIP, e.g. fd00::10. Empty answers NODATA.", Label: "Custom block IPv6", Group: GroupBlocking, Format: FormatIPv6},
	{Path: "blockPage.enabled", Type: TypeBool, Scope: ScopeCluster, Default: false, Help: "Show browsers a page that explains why a site was blocked, instead of a connection error. Turning it on starts the block page listener (listeners.blockPage) and allows the blockPage block response; choose that response in blocking.defaultResponse or a Policy. Blocked A and AAAA queries then point at blockPage.ipv4 and blockPage.ipv6; other types get NODATA.", RestartRequired: true, Label: "Block page enabled", Group: GroupBlockPage},
	{Path: "blockPage.ipv4", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "Address browsers are sent to for the block page, e.g. 192.168.1.10: this server's LAN address, or a VIP or load balancer in front of the block page. Blocked A queries are answered with it under the blockPage response. Empty picks the first specific address of listeners.blockPage, else of listeners.dns; when every listener binds all addresses (0.0.0.0, ::) there is none, and blockPage.enabled is refused.", Label: "Block page IPv4 address", Group: GroupBlockPage, Format: FormatIPv4},
	{Path: "blockPage.ipv6", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv6 address browsers are sent to for the block page, e.g. fd00::10; blocked AAAA queries are answered with it under the blockPage response. When blockPage.ipv4 is empty too, one is picked from the listeners as for IPv4; without an IPv6 address, AAAA queries get NODATA and browsers use IPv4.", Label: "Block page IPv6 address", Group: GroupBlockPage, Format: FormatIPv6},
	{Path: "blockPage.title", Type: TypeString, Scope: ScopeCluster, Default: "This site is blocked", Help: "Heading of the block page, e.g. This site is blocked (plain text, at most 200 characters).", Label: "Page title", Group: GroupBlockPage},
	{Path: "blockPage.message", Type: TypeString, Scope: ScopeCluster, Default: "The administrator of this network has blocked access to this domain.", Help: "Text under the heading that tells people why the site is blocked and what they can do (plain text, at most 2000 characters; HTML is shown as text).", Label: "Page message", Group: GroupBlockPage},
	{Path: "blockPage.contact", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "How people reach you, shown as \"Ask your administrator\": an email address, e.g. admin@home.example, or a link, e.g. https://wiki.home.example/dns. Empty leaves the line out.", Label: "Administrator contact", Group: GroupBlockPage},
	{Path: "blockPage.showDetails", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Show what blocked the name (the list, rule, blocked service, safe search or policy), looked up in the recent query log. Off shows only the domain, the client, the node and the time, which keeps your list choices private.", Label: "Show block details", Group: GroupBlockPage},
	{Path: "blockPage.dashboardURL", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "Address of the dashboard the block page links administrators to, e.g. https://192.168.1.10:8080; the link opens /queries?domain=... there. Empty builds it from the block page address and listeners.api; set it when the dashboard is reached under another name or through a reverse proxy.", Label: "Dashboard URL", Group: GroupBlockPage, Format: FormatURL},
	{Path: "blockPage.rateLimit", Type: TypeInt, Scope: ScopeCluster, Default: 120, Help: "Most block page requests one client address may make per minute, e.g. 120, with bursts up to the same number; beyond it the page answers 429 Too Many Requests. 0 removes the limit.", Label: "Requests per minute per client", Group: GroupBlockPage, Min: new(0), Max: new(100000)},
	{Path: "unblockRequests.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Let people ask for a blocked site to be unblocked: the block page shows a Request access form, and requests appear on the dashboard's Requests page for an administrator to approve or deny. Needs blockPage.enabled.", Label: "Unblock requests", Group: GroupBlockPage},
	{Path: "unblockRequests.rateLimit", Type: TypeInt, Scope: ScopeCluster, Default: 5, Help: "Most unblock requests one client may send per hour, e.g. 5; IPv6 clients are counted per /64. Asking again for the same name updates the pending request instead of adding one.", Label: "Unblock requests per hour per client", Group: GroupBlockPage, Min: new(1), Max: new(100)},
	{Path: "unblockRequests.maxPending", Type: TypeInt, Scope: ScopeCluster, Default: 1000, Help: "Most undecided unblock requests kept across the cluster, e.g. 1000. When reached, the block page says requests are paused until an administrator decides some.", Label: "Most pending unblock requests", Group: GroupBlockPage, Min: new(1), Max: new(10000)},
	{Path: "unblockRequests.pendingExpiry", Type: TypeDuration, Scope: ScopeCluster, Default: "336h", Help: "Undecided unblock requests expire after this long, e.g. 336h (14 days); at least 1h.", Label: "Pending requests expire after", Group: GroupBlockPage, Presets: []string{"24h", "168h", "336h", "720h"}},
	{Path: "unblockRequests.retention", Type: TypeDuration, Scope: ScopeCluster, Default: "720h", Help: "How long decided and expired unblock requests stay on the Requests page, counted from the decision, e.g. 720h (30 days); at least 1h. A time-limited approval is kept until it ends.", Label: "Keep decided requests for", Group: GroupBlockPage, Presets: []string{"168h", "720h", "2160h"}},
	{Path: "querylog.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Record the queries each node answers, for the dashboard's query log, statistics and charts; off records nothing and those pages stay empty. Each node keeps its own log (querylog.db with rollups, the stats API).", RestartRequired: true, Label: "Query log enabled", Group: GroupQuerylog},
	{Path: "querylog.sink", Type: TypeString, Scope: ScopeCluster, Default: "sqlite", Help: "Where queries are recorded: sqlite (default) keeps them in a database the dashboard reads; slog writes one JSON log line per query and stores nothing, for an external log system.", RestartRequired: true, Label: "Query log sink", Group: GroupQuerylog, Enum: []Option{{Value: "sqlite", Label: "SQLite", Help: "A database on each node (querylog.db, with rollups); needed for the dashboard's query log and statistics."}, {Value: "slog", Label: "Log lines", Help: "One JSON log line per query and nothing stored; the dashboard shows no query history."}}},
	{Path: "querylog.privacy", Type: TypeString, Scope: ScopeCluster, Default: "full", Help: "How much of each query is kept: full keeps everything; the other modes hide client addresses, query names or both, and nothing keeps only per-minute totals. Applied once when the query is recorded, so it cannot be undone for queries already logged. Only full stores the answer summary.", Label: "Query log privacy", Group: GroupQuerylog, Enum: []Option{{Value: "full", Label: "Full", Help: "Keep the client, the name and the answer summary of every query."}, {Value: "anonymize_client", Label: "Anonymize clients", Help: "Keep the names but hide client addresses, as set by querylog.anonymize."}, {Value: "hide_domains", Label: "Hide domains", Help: "Keep the clients but not the names they asked for."}, {Value: "hide_domains_clients", Label: "Hide domains and clients", Help: "Keep neither names nor clients."}, {Value: "nothing", Label: "Totals only", Help: "Keep no rows, only per-minute totals for the charts."}}},
	{Path: "querylog.anonymize", Type: TypeString, Scope: ScopeCluster, Default: "prefix", Help: "How client addresses are hidden under the anonymize_client privacy mode: prefix keeps only the network (IPv4 /24, IPv6 /48), so devices in one network look alike; pseudonym replaces each address with a code that is stable for a day and cannot be reversed.", Label: "Client anonymization", Group: GroupQuerylog, Enum: []Option{{Value: "prefix", Label: "Prefix", Help: "Keep only the network of each address: IPv4 /24, IPv6 /48."}, {Value: "pseudonym", Label: "Pseudonym", Help: "Replace each address with a code that stays the same for a day and cannot be reversed (HMAC with a random in-memory key rotated daily, so codes also change on restart)."}}},
	{Path: "querylog.retention.raw", Type: TypeDuration, Scope: ScopeCluster, Default: "720h", Help: "How long individual queries are kept, e.g. 720h (30 days); 0 keeps them until querylog.retention.maxDBBytes is reached. The totals behind the charts are kept longer: per minute for 35 days, per hour for 400 days, per day forever.", Label: "Keep query rows for", Group: GroupQuerylog, Presets: []string{"0s", "24h", "168h", "720h", "2160h"}},
	{Path: "querylog.retention.maxDBBytes", Type: TypeBytes, Scope: ScopeCluster, Default: "10GiB", Help: "Largest size of the query log database on each node, e.g. 10GiB; when it is exceeded the oldest queries are deleted first. 0 removes the cap.", Label: "Maximum query log size", Group: GroupQuerylog, Presets: []string{"0", "1GiB", "5GiB", "10GiB", "50GiB"}},
	{Path: "querylog.exclude.domains", Type: TypeStringList, Scope: ScopeCluster, Default: []string{}, Help: "Names that are never logged, e.g. *.home.arpa or connectivitycheck.gstatic.com: exact names, *.suffix for a domain and everything below it, or /regex/.", Label: "Domains never logged", Group: GroupQuerylog, Items: &Shape{Type: TypeString}},
	{Path: "querylog.exclude.clients", Type: TypeStringList, Scope: ScopeCluster, Default: []string{}, Help: "Clients whose queries are never logged, e.g. 192.168.1.20, 10.0.0.0/8 or a client name such as nas: IP addresses, CIDR prefixes or client names.", Label: "Clients never logged", Group: GroupQuerylog, Items: &Shape{Type: TypeString}},
	{Path: "querylog.exclude.sudn", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Leave out names that never leave the local network, which keeps local discovery noise out of the log: localhost, .local, .home.arpa and private and link-local reverse zones (special-use names, RFC 6761).", Label: "Skip special-use names", Group: GroupQuerylog},
	{Path: "database.dsn", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Optional PostgreSQL 16 or newer database that keeps the audit events, query log and statistics of every node in one place, so every node shows the same history and it outlives the nodes; empty (default) keeps them on each node. A connection string, e.g. postgres://kudurru@db.example:5432/kudurru?sslmode=verify-full; it may hold a password, so set it in the file or with KUDURRU_DATABASE__DSN_FILE. DNS never depends on it: nodes keep their own copy and send it when the database is reachable. docs/deploy/postgres.md.", HostOnly: true, RestartRequired: true, WriteOnly: true, Label: "Database connection", Group: GroupDatabase},
	{Path: "database.maxConns", Type: TypeInt, Scope: ScopeNode, Default: 4, Help: "Most connections each node opens to the central database, e.g. 4.", HostOnly: true, RestartRequired: true, Label: "Database connections per node", Group: GroupDatabase, Min: new(1), Max: new(32)},
	{Path: "database.querylog.ship", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "With database.dsn set, every node copies its query log and statistics into the central database, so the dashboard's All nodes view shows every node's queries and totals whichever node serves it. Each node keeps its own querylog.db too. Needs querylog.sink sqlite.", RestartRequired: true, Label: "Send the query log to the database", Group: GroupDatabase},
	{Path: "database.querylog.facts", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Copy the individual queries, not only the statistics. At 1000 queries per second that is about 25 GB a day before retention; off keeps the cluster-wide charts, totals and top lists but the All nodes query log is empty.", RestartRequired: true, Label: "Send individual queries", Group: GroupDatabase},
	{Path: "database.querylog.retention.raw", Type: TypeDuration, Scope: ScopeCluster, Default: "168h", Help: "How long individual queries are kept in the central database, e.g. 168h (7 days); whole days are dropped. 0 keeps them. Statistics are kept like on the nodes: per minute 35 days, per hour 400 days, per day forever.", RestartRequired: true, Label: "Keep queries in the database for", Group: GroupDatabase, Presets: []string{"24h", "168h", "720h"}},
	{Path: "blocking.refreshInterval", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "How often blocklists are downloaded again when a list sets no refreshInterval of its own, e.g. 24h; at least 1m. In a cluster the leader downloads and the other nodes copy its result by digest.", RestartRequired: true, Label: "List refresh interval", Group: GroupBlocking, Presets: []string{"1h", "6h", "12h", "24h", "168h"}},
	{Path: "blocking.initStrategy", Type: TypeString, Scope: ScopeCluster, Default: "fast", Help: "What happens at start while blocklists load: fast (default) serves at once using the copies saved on disk; blocking waits until every list is loaded; failOnError also exits when a list cannot be loaded.", RestartRequired: true, Label: "List loading at start", Group: GroupBlocking, Enum: []Option{{Value: "fast", Label: "Fast", Help: "Serve at once using the lists saved on disk, and load fresh copies in the background."}, {Value: "blocking", Label: "Wait for lists", Help: "Report not ready until every list is loaded."}, {Value: "failOnError", Label: "Fail on error", Help: "Wait for every list, and exit with an error when one fails to load."}}},
	{Path: "blocking.listsDir", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Directory where downloaded blocklists are saved so the next start can use them at once. Empty uses <node.dataDir>/lists. Files are named by the sha256 of the list URL.", HostOnly: true, RestartRequired: true, Label: "List cache directory", Group: GroupBlocking},
	{Path: "blocking.allowLocalListURLs", Type: TypeBool, Scope: ScopeNode, Default: false, Help: "Allow blocklist URLs that point at this machine or at special addresses: loopback, link-local (169.254.0.0/16, fe80::/10, where cloud metadata services live), unspecified and multicast. Off by default so whoever can add a BlockList cannot make the node fetch internal services (SSRF). Lists on private LAN addresses such as 192.168.1.5 are always allowed.", HostOnly: true, RestartRequired: true, Label: "Allow local list URLs", Group: GroupBlocking},
	{Path: "blocking.catalogue.url", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "Leave empty to use the blocked-services and safe-search catalogue built into kudurru. A URL, e.g. https://example.com/kudurru-catalogue.json, replaces it with your own document (schema version 2, or 1, docs/deploy/blocked-services.md). The leader downloads it and the other nodes copy the leader's; a document that fails validation leaves the current catalogue in service.", RestartRequired: true, Label: "Catalogue URL", Group: GroupBlocking, Format: FormatURL},
	{Path: "blocking.catalogue.adguardServicesURL", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "Off when empty. Opt-in: a URL of AdGuard's HostlistsRegistry blocked-services file, e.g. https://raw.githubusercontent.com/AdguardTeam/HostlistsRegistry/main/assets/services.json, adds its services (about 140) to the catalogue next to kudurru's own; kudurru's entries win when both have an ID. That data is GPL-3.0 licensed: kudurru does not ship it, your instance downloads it (the leader, on the catalogue refresh interval; the other nodes copy the leader's), and the dashboard marks those services as from AdGuard (GPL-3.0). Check that the licence suits how you use and share the data. docs/deploy/blocked-services.md.", RestartRequired: true, Label: "AdGuard services URL (GPL-3.0, opt-in)", Group: GroupBlocking, Format: FormatURL},
	{Path: "blocking.catalogue.refreshInterval", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "How often the catalogue at blocking.catalogue.url and the AdGuard services at blocking.catalogue.adguardServicesURL are checked for a new version, e.g. 24h; at least 1m. A conditional GET, so an unchanged document is not downloaded again.", RestartRequired: true, Label: "Catalogue refresh interval", Group: GroupBlocking, Presets: []string{"6h", "24h", "168h"}},
	{Path: "ratelimit.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Limit how many queries per minute one address or network may send over UDP, which blunts floods and reflection attacks. TCP, DoT, DoH, DoQ and DoH3 are never limited, nor loopback clients and the health name.", Label: "Rate limiting enabled", Group: GroupRateLimit},
	{Path: "ratelimit.ipv4", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"32": 600, "24": 6000}, Help: "Queries per minute allowed per IPv4 address or network, by prefix length: 32 is one address (default 600), 24 a /24 network (default 6000). Entries merge with the defaults key by key, and 0 turns a level off. Each prefix has a token bucket holding one minute's allowance.", Label: "IPv4 limits (queries per minute)", Group: GroupRateLimit, Keys: &Shape{Type: TypeInt, Min: new(0), Max: new(32)}, Values: rateLevelValue},
	{Path: "ratelimit.ipv6", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"128": 600, "64": 6000, "56": 12000}, Help: "Queries per minute allowed per IPv6 address or network, by prefix length: 128 is one address (default 600), 64 a /64 network (6000), 56 a /56 (12000). Entries merge with the defaults key by key, and 0 turns a level off.", Label: "IPv6 limits (queries per minute)", Group: GroupRateLimit, Keys: &Shape{Type: TypeInt, Min: new(0), Max: new(128)}, Values: rateLevelValue},
	{Path: "ratelimit.slip", Type: TypeInt, Scope: ScopeCluster, Default: 50, Help: "Share of over-limit queries, in percent from 0 to 100, answered with an empty truncated reply (TC=1) instead of being dropped, e.g. 50; real clients then retry over TCP, which is not limited. 0 drops them all, 100 answers them all.", Label: "Slip (percent answered TC=1)", Group: GroupRateLimit, Min: new(0), Max: new(100)},
	{Path: "ratelimit.maxBuckets", Type: TypeInt, Scope: ScopeCluster, Default: 65536, Help: "Most addresses and networks tracked at once, e.g. 65536 (0 means 65536); the least recently seen is forgotten first and starts again with a full allowance.", Label: "Tracked prefixes", Group: GroupRateLimit, Min: new(0)},
	{Path: "ratelimit.exempt", Type: TypeStringList, Scope: ScopeCluster, Default: []string{}, Help: "Addresses or networks that are never rate limited, e.g. 192.168.1.0/24 for your LAN or the address of a busy resolver that forwards to kudurru. Loopback always is.", Label: "Exempt clients", Group: GroupRateLimit, Items: &Shape{Type: TypeString, Format: FormatCIDR}},
	{Path: "objects", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Zones, lists, groups and policies declared in this file, in the form of Kubernetes manifests (apiVersion, kind, metadata, spec). They are re-applied on every start; edit them here, since they are managed by file:kudurru.yaml and the dashboard cannot change them.", HostOnly: true, RestartRequired: true, Label: "Inline objects", Group: GroupNode, Items: objectShape},
	{Path: "settingsFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of a file for the cluster-wide settings, e.g. /etc/kudurru/settings.yaml, read on top of this file and watched, so edits apply without a restart. In Docker, changes made on the Settings page are saved into it; without one they are lost on restart. KUDURRU_SETTINGS_FILE takes precedence, and the Helm chart mounts /etc/kudurru/settings/settings.yaml. Empty turns the layer off (D25).", HostOnly: true, RestartRequired: true, Label: "Settings file", Group: GroupNode},
	{Path: "objectsDir", Type: TypeString, Scope: ScopeNode, Default: "/etc/kudurru/objects.d", Help: "Directory of *.yaml files with zones, lists, groups and policies, e.g. /etc/kudurru/objects.d. It is watched: adding or editing a file applies it, and deleting a document deletes the object. The objects are managed by file:<dir>/<name>, so the dashboard cannot change them.", HostOnly: true, RestartRequired: true, Label: "Objects directory", Group: GroupNode},
	{Path: "tsigKeys", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Shared keys that authenticate zone transfers and NOTIFY between kudurru and other DNS servers; only needed when you transfer zones. Each key has a name, an algorithm and a file holding the secret, and a zone lists the keys it accepts in spec.policy.transferTSIGKeyRefs (TSIG, RFC 8945).", HostOnly: true, RestartRequired: true, WriteOnly: true, Label: "TSIG keys", Group: GroupZones, Items: tsigKeyShape},
	{Path: "notify.source", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Local IP address that NOTIFY messages to secondary servers are sent from, e.g. 192.168.1.10; empty lets the system choose. Secondaries accept NOTIFY only from an address they list as a primary.", HostOnly: true, RestartRequired: true, Label: "NOTIFY source address", Group: GroupZones, Format: FormatIP},
	{Path: "externalDNS.webhook.enabled", Type: TypeBool, Scope: ScopeNode, Default: false, Help: "Let external-dns, the Kubernetes tool, publish DNS records into kudurru through its webhook provider protocol (GET /, GET and POST /records, POST /adjustendpoints), served on externalDNS.webhook.listen.", HostOnly: true, RestartRequired: true, Label: "external-dns webhook enabled", Group: GroupIntegrations},
	{Path: "externalDNS.webhook.listen", Type: TypeString, Scope: ScopeNode, Default: "127.0.0.1:8888", Help: "Address and port of the external-dns webhook, e.g. 127.0.0.1:8888. It has no authentication, so the default answers only on this machine (external-dns as a sidecar or on the same host); a wider address lets every client that reaches it write records. The Helm chart renders :<port> behind its NetworkPolicy.", HostOnly: true, RestartRequired: true, Label: "external-dns webhook address", Group: GroupIntegrations, Format: FormatHostPort, Presets: []string{"127.0.0.1:8888", ":8888"}},
	{Path: "externalDNS.webhook.namespace", Type: TypeString, Scope: ScopeNode, Default: "external-dns", Help: "Namespace the record sets from external-dns are stored in, e.g. external-dns: one DNSRecordSet per DNS name, managed by integration:external-dns.", HostOnly: true, RestartRequired: true, Label: "external-dns record namespace", Group: GroupIntegrations},
	{Path: "externalDNS.webhook.zoneSelector", Type: TypeMap, Scope: ScopeNode, Default: map[string]any{}, Help: "Labels a primary zone must carry for external-dns to write into it, e.g. external-dns: enabled (a label selector, key: value); empty allows every primary zone.", HostOnly: true, RestartRequired: true, Label: "external-dns zone selector", Group: GroupIntegrations, Keys: &Shape{Type: TypeString}, Values: &Shape{Type: TypeString}},
	{Path: "auth.localLogin", Type: TypeString, Scope: ScopeCluster, Default: "enabled", Help: "Whether people can sign in to the dashboard with a username and password: enabled for everyone, admins-only for builtin admins, or disabled (identity providers only). `kudurru admin` on the server's admin socket always works, so you cannot lock yourself out. Gates POST /api/v1/auth/login.", RestartRequired: true, Label: "Local password login", Group: GroupSecurity, Enum: []Option{{Value: "enabled", Label: "Enabled", Help: "Every local user may sign in with a password."}, {Value: "admins-only", Label: "Admins only", Help: "Only builtin admins may sign in with a password; everyone else uses an identity provider."}, {Value: "disabled", Label: "Disabled", Help: "No password sign-in; use an identity provider. kudurru admin over the admin socket still works."}}},
	{Path: "auth.bootstrap.adminPasswordFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Path of a file holding the password of the first admin account, created on the first start, e.g. /run/secrets/kudurru-admin-password. The KUDURRU_ADMIN_PASSWORD and KUDURRU_ADMIN_PASSWORD_FILE environment variables take precedence; with none of them, a password is generated and printed once.", HostOnly: true, RestartRequired: true, Label: "First-boot admin password file", Group: GroupSecurity},
	{Path: "auth.session.idleTimeout", Type: TypeDuration, Scope: ScopeCluster, Default: "30m", Help: "Sign a dashboard user out after this long without activity, e.g. 30m; at least 1m. Every request starts the time again.", RestartRequired: true, Label: "Session idle timeout", Group: GroupSecurity, Presets: []string{"15m", "30m", "1h", "8h"}},
	{Path: "auth.session.lifetime", Type: TypeDuration, Scope: ScopeCluster, Default: "12h", Help: "Longest a dashboard sign-in lasts however active it is, e.g. 12h; then the user signs in again and their roles are looked up afresh. At least auth.session.idleTimeout.", RestartRequired: true, Label: "Session lifetime", Group: GroupSecurity, Presets: []string{"8h", "12h", "24h", "168h"}},
	{Path: "auth.session.secure", Type: TypeString, Scope: ScopeCluster, Default: "auto", Help: "Whether browsers send the session cookie only over HTTPS (the cookie's Secure attribute). auto (default) decides per request and suits most setups; always is for a proxy that terminates TLS without telling kudurru; never is only for plain HTTP on a network you trust.", RestartRequired: true, Label: "Secure session cookie", Group: GroupSecurity, Enum: []Option{{Value: "auto", Label: "Auto", Help: "Secure when TLS terminates at kudurru or a trusted proxy reports https."}, {Value: "always", Label: "Always", Help: "Always Secure; for a proxy that terminates TLS without sending X-Forwarded-Proto."}, {Value: "never", Label: "Never", Help: "Never Secure: for plain HTTP only, and the cookie can be read on the network."}}},
	{Path: "auth.tokens.maxLifetime", Type: TypeDuration, Scope: ScopeCluster, Default: "0s", Help: "Longest lifetime of a new API token, e.g. 2160h (90 days); a token created without an expiry gets this one. 0 (default) allows tokens that never expire.", RestartRequired: true, Label: "Maximum token lifetime", Group: GroupSecurity, Presets: []string{"0s", "720h", "2160h", "8760h"}},
	{Path: "audit.store", Type: TypeString, Scope: ScopeCluster, Default: "auto", Help: "Where audit events are kept so every node can show them: auto (default) uses database when database.dsn is set, else auditlog with the kube store driver, else local; database keeps every node's events in the central database (one hash chain); auditlog keeps them in AuditLog objects in the release namespace, read by every replica and kept across restarts; local keeps them on each node (state.db with the sqlite driver, audit-spool.db with kube), so a replica shows only its own. Every event is also a \"log\":\"audit\" log line.", HostOnly: true, RestartRequired: true, Label: "Audit log store", Group: GroupSecurity, Enum: []Option{{Value: "auto", Label: "Automatic", Help: "database when database.dsn is set, auditlog in Kubernetes (store.driver kube), local otherwise."}, {Value: "database", Label: "Central database", Help: "Every node's events in the central PostgreSQL database (database.dsn), in one hash chain. Each node also keeps its own copy and sends it when the database is reachable."}, {Value: "auditlog", Label: "AuditLog objects", Help: "Batches of events in AuditLog objects (audit.kudurru.io) in the release namespace: the same on every replica, kept across restarts. Needs store.driver kube and the kudurru-crds chart."}, {Value: "local", Label: "On each node", Help: "Each node keeps the events it records; the dashboard shows those of the node that answers."}}},
	{Path: "audit.retention", Type: TypeDuration, Scope: ScopeCluster, Default: "9600h", Help: "How long stored audit events are kept, e.g. 9600h (400 days); at least 24h. The log lines are kept by your log system, not by kudurru.", HostOnly: true, RestartRequired: true, Label: "Keep audit events for", Group: GroupSecurity, Presets: []string{"720h", "2160h", "9600h"}},
	{Path: "auth.identityProviders", Type: TypeList, Scope: ScopeCluster, Default: []map[string]any{}, Help: "Sign-in to the dashboard through an OpenID Connect provider such as Google, Microsoft Entra ID, Keycloak, Authentik or Dex; the provider's ID tokens also work as bearer tokens for the API. Each entry needs an id, issuer, clientID, clientSecretFile and redirectURL; scopes, claimValidationRules, claimMappings and provisioning are optional.", HostOnly: true, RestartRequired: true, Label: "Identity providers", Group: GroupSecurity, Items: identityProviderShape},
	{Path: "auth.kubernetes.mode", Type: TypeString, Scope: ScopeCluster, Default: "sar", Help: "How Kubernetes service-account tokens are authorized: sar asks the apiserver with a SubjectAccessReview on kudurru.io resources. impersonate is planned for v0.2.", HostOnly: true, RestartRequired: true, Label: "Kubernetes authorization", Group: GroupSecurity, Enum: []Option{{Value: "sar", Label: "SubjectAccessReview", Help: "Ask the apiserver with a SubjectAccessReview whether the service account may act on the kudurru.io resource."}}, normalize: allowPlannedKubernetesMode},
	{Path: "auth.adminSocket.path", Type: TypeString, Scope: ScopeNode, Default: "/run/kudurru/kudurru.sock", Help: "Path of the local socket of the `kudurru admin` command (reset-password, create-token, list-users), e.g. /run/kudurru/kudurru.sock: the way back in when every admin password is lost. Only uid 0, the server's own user and auth.adminSocket.group may use it. Empty turns it off.", HostOnly: true, RestartRequired: true, Label: "Admin socket path", Group: GroupSecurity},
	{Path: "auth.adminSocket.group", Type: TypeInt, Scope: ScopeNode, Default: -1, Help: "Group id (gid) whose members may also use the admin socket, e.g. 1000; -1 (default) allows only root and the server's own user.", HostOnly: true, RestartRequired: true, Label: "Admin socket group (gid)", Group: GroupSecurity, Min: new(-1)},
	{Path: "rbac.bindings", Type: TypeList, Scope: ScopeCluster, Default: []map[string]any{}, Help: "Who may do what: each binding grants a role (viewer, auditor, operator, zone-admin or admin) to users, groups or service accounts, for the whole cluster or one zone. RoleBinding objects add to these.", HostOnly: true, RestartRequired: true, Label: "Role bindings", Group: GroupSecurity, Items: roleBindingShape},
	{Path: "log.level", Type: TypeString, Scope: ScopeNode, Default: "info", Help: "How much the server logs: info (default) for normal operation, debug for troubleshooting, warn or error for less. Per-component levels follow a comma, e.g. info,dns=debug.", Label: "Log level", Group: GroupLogging, Enum: []Option{{Value: "debug", Label: "Debug", Help: "Everything, including detail for troubleshooting; noisy."}, {Value: "info", Label: "Info", Help: "Start-up, changes and notable events; the default."}, {Value: "warn", Label: "Warning", Help: "Only problems that need attention, and errors."}, {Value: "error", Label: "Error", Help: "Only errors."}}, normalize: baseLogLevel},
	{Path: "log.format", Type: TypeString, Scope: ScopeNode, Default: "json", Help: "Format of the log lines: json (default) for log collectors, text for reading in a terminal.", RestartRequired: true, Label: "Log format", Group: GroupLogging, Enum: []Option{{Value: "json", Label: "JSON", Help: "One JSON object per line; for log collectors."}, {Value: "text", Label: "Text", Help: "key=value lines; easier to read in a terminal."}}},
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
