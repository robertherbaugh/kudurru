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
	// the node ConfigMap) and need a process restart in Docker.
	RestartRequired bool
	// WriteOnly keys are never read back through the API (secrets).
	WriteOnly bool
}

// Schema is the settings metadata table. Every leaf of Config must have an
// entry; TestSchemaMatchesConfig enforces it.
var Schema = []Key{
	{Path: "node.name", Type: TypeString, Scope: ScopeNode, Default: "", Help: "Node identity; defaults to the hostname.", HostOnly: true, RestartRequired: true},
	{Path: "node.dataDir", Type: TypeString, Scope: ScopeNode, Default: "/var/lib/kudurru", Help: "Directory for state.db, querylog.db, local.db, lists and keys.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.dns", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{{"address": "0.0.0.0:53", "protocols": []string{"udp", "tcp"}}}, Help: "DNS listeners: address and protocols (udp, tcp).", HostOnly: true, RestartRequired: true},
	{Path: "listeners.dot", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-TLS listeners (RFC 7858, ALPN dot): address and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.doh", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-HTTPS listeners (RFC 8484, HTTP/2 and HTTP/1.1): address, path (default /dns-query) and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.doq", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-QUIC listeners (RFC 9250, udp, ALPN doq): address and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.doh3", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "DNS-over-HTTPS over HTTP/3 listeners (udp, ALPN h3): address, path (default /dns-query) and tls {certFile, keyFile}.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.api.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8080", Help: "HTTP API and UI listener.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.ops.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8081", Help: "Unauthenticated health listener (/healthz/*).", HostOnly: true, RestartRequired: true},
	{Path: "listeners.metrics.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:9153", Help: "Prometheus /metrics listener.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.proxyProtocolFrom", Type: TypeStringList, Scope: ScopeNode, Default: []string{}, Help: "Exact peers (CIDRs, normally /32) allowed to send PROXYv2 headers; loopback only when dnsdist shares the network namespace.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.proxyProtocolSecretFile", Type: TypeString, Scope: ScopeNode, Default: "", Help: "File holding the PROXYv2 TLV 0xE0 secret shared with the dnsdist supervisor (at least 32 bytes, e.g. openssl rand -hex 32); required when proxyProtocolFrom names a non-loopback peer.", HostOnly: true, RestartRequired: true},
	{Path: "store.driver", Type: TypeString, Scope: ScopeNode, Default: "sqlite", Help: "Store driver: sqlite (state.db under node.dataDir) or kube (CRDs via the apiserver).", HostOnly: true, RestartRequired: true},
	{Path: "store.kube.startupTimeout", Type: TypeDuration, Scope: ScopeNode, Default: "15s", Help: "How long to wait for the apiserver watches to sync before serving from local.db.", HostOnly: true, RestartRequired: true},
	{Path: "store.kube.snapshot.maxAge", Type: TypeDuration, Scope: ScopeNode, Default: "24h", Help: "Oldest local.db snapshot that may still answer primary zones; older snapshots answer SERVFAIL while forwarding continues.", HostOnly: true, RestartRequired: true},
	{Path: "upstreams.groups", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"default": map[string]any{"servers": []string{"1.1.1.1:53", "9.9.9.9:53"}, "strategy": "parallel_best", "timeout": "2s"}}, Help: "Named upstream groups: servers, strategy (parallel_best, strict, random, rtt_weighted), timeout."},
	{Path: "upstreams.bootstrap", Type: TypeStringList, Scope: ScopeCluster, Default: []string{"1.1.1.1:53"}, Help: "Plain resolvers used to resolve upstream hostnames (DoT/DoH)."},
	{Path: "upstreams.conditional", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{}, Help: "Suffix to upstream group map for conditional forwarding (longest suffix wins); a key may list several suffixes in dnsmasq form, \"[/corp.example/10.in-addr.arpa/]\". Conditional answers are never cached."},
	{Path: "upstreams.ecs.mode", Type: TypeString, Scope: ScopeCluster, Default: "strip", Help: "EDNS Client Subnet on upstream queries: strip (never send one; privacy default), forward (pass the client's own option), add (forward the client's option, else derive one from a public client address)."},
	{Path: "upstreams.ecs.ipv4Prefix", Type: TypeInt, Scope: ScopeCluster, Default: 24, Help: "Longest IPv4 source prefix sent upstream in ECS (0-32)."},
	{Path: "upstreams.ecs.ipv6Prefix", Type: TypeInt, Scope: ScopeCluster, Default: 56, Help: "Longest IPv6 source prefix sent upstream in ECS (0-128)."},
	{Path: "cache.maxEntries", Type: TypeInt, Scope: ScopeCluster, Default: 200000, Help: "Maximum number of cached responses (LRU beyond it); 0 disables the cache."},
	{Path: "cache.minTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "5s", Help: "Lower clamp for positive answer TTLs."},
	{Path: "cache.maxTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "24h", Help: "Upper clamp for answer TTLs."},
	{Path: "cache.negativeTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "5m", Help: "Upper clamp for NXDOMAIN/NODATA TTLs (RFC 2308 SOA minimum otherwise)."},
	{Path: "cache.prefetch.threshold", Type: TypeInt, Scope: ScopeCluster, Default: 5, Help: "Hits within prefetch.window that make an entry hot enough to refresh before it expires; 0 disables prefetch."},
	{Path: "cache.prefetch.window", Type: TypeDuration, Scope: ScopeCluster, Default: "2h", Help: "Window over which prefetch hits are counted."},
	{Path: "cache.serveStale.ttl", Type: TypeDuration, Scope: ScopeCluster, Default: "72h", Help: "How long past expiry an answer may be served stale when every upstream fails (RFC 8767); 0 disables serve-stale."},
	{Path: "cache.serveStale.answerTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "30s", Help: "TTL of stale answers (RFC 8767)."},
	{Path: "cache.serveStale.maxWait", Type: TypeDuration, Scope: ScopeCluster, Default: "1800ms", Help: "How long a query for an expired entry waits for the upstream refresh before the stale answer is sent (RFC 8767 client response timer)."},
	{Path: "blocking.enabled", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Apply blocklists to recursive queries; false answers everything unfiltered (temporary disable is a runtime toggle, not this key)."},
	{Path: "blocking.defaultResponse", Type: TypeString, Scope: ScopeCluster, Default: "zeroIP", Help: "Block response when neither the Policy (blockMode) nor the BlockList (response) sets one: zeroIP (0.0.0.0 / ::, other types NODATA), nxdomain, customIP (blocking.customIP), refused (discouraged: stub resolvers fail over)."},
	{Path: "blocking.blockTTL", Type: TypeDuration, Scope: ScopeCluster, Default: "10m", Help: "TTL of block responses when the Policy sets no blockTTL; short so a one-click allow takes effect quickly."},
	{Path: "blocking.allowDegraded", Type: TypeBool, Scope: ScopeCluster, Default: true, Help: "Report ready and serve while referenced blocklists are not loaded yet (queries are filtered by the lists that are loaded)."},
	{Path: "blocking.customIP.v4", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv4 answer for blocked A queries when the response is customIP; empty answers NODATA."},
	{Path: "blocking.customIP.v6", Type: TypeString, Scope: ScopeCluster, Default: "", Help: "IPv6 answer for blocked AAAA queries when the response is customIP; empty answers NODATA."},
	{Path: "objects", Type: TypeList, Scope: ScopeNode, Default: []map[string]any{}, Help: "Inline CRD-shaped objects (zones, lists, groups, policies); file-managed as file:kudurru.yaml and re-applied on start.", HostOnly: true, RestartRequired: true},
	{Path: "objectsDir", Type: TypeString, Scope: ScopeNode, Default: "/etc/kudurru/objects.d", Help: "Directory of *.yaml object files, file-managed as file:<dir>/<name> and re-applied on change; deleting a document deletes the object.", HostOnly: true, RestartRequired: true},
	{Path: "log.level", Type: TypeString, Scope: ScopeNode, Default: "info", Help: "Log level: debug, info, warn, error. Per-role overrides as info,dns=debug."},
	{Path: "log.format", Type: TypeString, Scope: ScopeNode, Default: "json", Help: "Log format: json or text."},
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
