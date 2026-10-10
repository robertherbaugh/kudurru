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
	{Path: "listeners.api.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8080", Help: "HTTP API and UI listener.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.ops.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:8081", Help: "Unauthenticated health listener (/healthz/*).", HostOnly: true, RestartRequired: true},
	{Path: "listeners.metrics.address", Type: TypeString, Scope: ScopeNode, Default: "0.0.0.0:9153", Help: "Prometheus /metrics listener.", HostOnly: true, RestartRequired: true},
	{Path: "listeners.proxyProtocolFrom", Type: TypeStringList, Scope: ScopeNode, Default: []string{}, Help: "Exact peers (CIDRs, normally /32) allowed to send PROXYv2 headers; loopback only when dnsdist shares the network namespace.", HostOnly: true, RestartRequired: true},
	{Path: "upstreams.groups", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{"default": map[string]any{"servers": []string{"1.1.1.1:53", "9.9.9.9:53"}, "strategy": "parallel_best", "timeout": "2s"}}, Help: "Named upstream groups: servers, strategy (parallel_best, strict, random, rtt_weighted), timeout."},
	{Path: "upstreams.bootstrap", Type: TypeStringList, Scope: ScopeCluster, Default: []string{"1.1.1.1:53"}, Help: "Plain resolvers used to resolve upstream hostnames (DoT/DoH)."},
	{Path: "upstreams.conditional", Type: TypeMap, Scope: ScopeCluster, Default: map[string]any{}, Help: "Suffix to upstream group map for conditional forwarding."},
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
