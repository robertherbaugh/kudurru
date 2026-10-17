// Package integration is the extension seam: in-tree Go integrations
// (Tier A) implement these interfaces and register at init; out-of-process
// HTTP integrations (Tier B) speak the external-dns webhook contract plus
// kudurru extensions (dev-docs/design-record.md §9). Integrations never run on
// the query path.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Capability is one thing an integration can do.
type Capability string

// Capabilities.
const (
	CapRecordSink   Capability = "RecordSink"   // outbound records (Cloudflare)
	CapClientSource Capability = "ClientSource" // inbound inventory (UniFi)
	CapHook         Capability = "Hook"         // event subscriber
	CapActions      Capability = "Actions"      // operator-invokable actions
)

// SecretKey names a spec field that is a secret reference.
type SecretKey struct {
	Path string `json:"path"`
	// Verify describes how Check validates it (e.g. "cloudflare:tokens/verify").
	Verify string `json:"verify,omitempty"`
}

// Manifest describes an integration type. ConfigSchema also becomes the
// Integration CRD one-of block and the UI form.
type Manifest struct {
	Type         string          `json:"type"`
	DisplayName  string          `json:"displayName"`
	Version      string          `json:"version"`
	Capabilities []Capability    `json:"capabilities"`
	IOClass      string          `json:"ioClass"` // cloud_polling | local_polling | local_push
	ConfigSchema json.RawMessage `json:"configSchema"`
	SecretKeys   []SecretKey     `json:"secretKeys,omitempty"`
	Docs         string          `json:"docs,omitempty"`
}

// Status is what Check reports.
type Status struct {
	Authenticated bool
	Reachable     bool
	Message       string
	Provider      map[string]string
}

// SecretResolver resolves secret references from the carrier (Kubernetes
// Secret or file) without exposing values in spec or status.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Integration is the lifecycle every integration implements (Caddy style:
// New → strict unmarshal → Provision → Validate → Start → Stop).
type Integration interface {
	Manifest() Manifest
	Provision(ctx context.Context, host Host, spec json.RawMessage, secrets SecretResolver) error
	Validate(ctx context.Context) error
	Check(ctx context.Context) (Status, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Endpoint is the external-dns wire shape; kudurru's own records are the
// superset in api/v1alpha1.
type Endpoint = v1alpha1.Endpoint

// Changes is the external-dns plan delta.
type Changes struct {
	Create    []Endpoint
	UpdateOld []Endpoint
	UpdateNew []Endpoint
	Delete    []Endpoint
}

// ApplyResult returns provider identifiers for the ledger.
type ApplyResult struct {
	ProviderIDs map[string]string // key: dnsName/type/setIdentifier
}

// DomainFilter restricts a sink to zones it may write.
type DomainFilter struct {
	Include []string
	Exclude []string
}

// RecordSink publishes records to an external provider.
type RecordSink interface {
	DomainFilter() DomainFilter
	Records(ctx context.Context, zone string) ([]Endpoint, error)
	AdjustEndpoints(ctx context.Context, eps []Endpoint) ([]Endpoint, error)
	ApplyChanges(ctx context.Context, zone string, ch Changes) (ApplyResult, error)
}

// Network is a client network (VLAN) from an inventory source.
type Network struct {
	Name   string
	VLAN   int
	CIDR   string
	Domain string
}

// Client is one inventory entry.
type Client struct {
	ID         string
	MACs       []string
	IPs        []string
	Name       string
	Hostname   string
	Network    Network
	FixedIP    string
	Tags       []string
	LastSeen   time.Time
	Attributes map[string]string
}

// ClientSource provides inventory.
type ClientSource interface {
	ListClients(ctx context.Context) ([]Client, error)
	ListNetworks(ctx context.Context) ([]Network, error)
}

// Event is a lifecycle event delivered at-least-once.
type Event struct {
	Topic   string
	Time    time.Time
	Payload json.RawMessage
}

// Hook subscribes to events.
type Hook interface {
	Topics() []string
	Handle(ctx context.Context, ev Event) error
}

// ActionSpec describes an operator-invokable action.
type ActionSpec struct {
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params"` // JSON Schema
	Verb   string          `json:"verb"`   // RBAC verb required (integrations.act)
}

// ActionProvider exposes actions.
type ActionProvider interface {
	Actions() []ActionSpec
	Invoke(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error)
}

// Host is what the host lends an integration. The concrete readers and
// writers arrive with the integration runner in v0.2; the interface is the
// seam.
type Host interface {
	Logger() *slog.Logger
	Metrics() prometheus.Registerer
	// Resolve re-enters the pipeline (Technitium's DirectQuery idea).
	Resolve(ctx context.Context, q dns.Question) (*dns.Msg, error)
	// SetCondition writes a status condition on the Integration object.
	SetCondition(ctx context.Context, typ, reason, message string, ok bool) error
}

// Constructor builds an unprovisioned integration.
type Constructor func() Integration

var (
	regMu    sync.RWMutex
	registry = map[string]entry{}
)

type entry struct {
	manifest Manifest
	ctor     Constructor
}

// Register adds an integration type. It panics on duplicates, so a typo in
// two packages fails at startup rather than at configuration time.
func Register(m Manifest, ctor Constructor) {
	regMu.Lock()
	defer regMu.Unlock()
	if m.Type == "" {
		panic("integration: manifest type is empty")
	}
	if _, dup := registry[m.Type]; dup {
		panic(fmt.Sprintf("integration: type %q registered twice", m.Type))
	}
	registry[m.Type] = entry{manifest: m, ctor: ctor}
}

// Lookup returns the constructor for a type.
func Lookup(typ string) (Manifest, Constructor, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	e, ok := registry[typ]
	return e.manifest, e.ctor, ok
}

// Types lists registered types in sorted order.
func Types() []Manifest {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Manifest, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}
