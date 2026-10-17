package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

const dhcpBase = "/node/dhcp/v4"

// DHCPPoolOptions contains only mutable scope options; topology is host-owned.
type DHCPPoolOptions struct {
	NTPServers      []string                    `json:"ntpServers,omitempty"`
	DomainSearch    []string                    `json:"domainSearch,omitempty"`
	ClasslessRoutes []config.DHCPClasslessRoute `json:"classlessRoutes,omitempty"`
	BootServer      string                      `json:"bootServer,omitempty"`
	BootServerName  string                      `json:"bootServerName,omitempty"`
	BootFile        string                      `json:"bootFile,omitempty"`
	BootProfiles    []config.DHCPBootProfile    `json:"bootProfiles,omitempty"`

	RangeStart    string   `json:"rangeStart" format:"ipv4"`
	RangeEnd      string   `json:"rangeEnd" format:"ipv4"`
	Router        string   `json:"router" format:"ipv4"`
	DNSServers    []string `json:"dnsServers" minItems:"1" maxItems:"63" nullable:"false"`
	DomainName    string   `json:"domainName"`
	LeaseDuration string   `json:"leaseDuration"`
	RegisterDNS   bool     `json:"registerDNS"`
	DNSTTL        string   `json:"dnsTTL"`
}

// DHCPReservation assigns an address and optional hostname to a MAC address.
type DHCPReservation struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip" format:"ipv4"`
	Hostname string `json:"hostname"`
}

// DHCPTrustedRelay identifies a host-configured relay and gateway pair.
type DHCPTrustedRelay struct {
	SourcePort       uint16 `json:"sourcePort,omitempty"`
	LinkSelection    string `json:"linkSelection,omitempty"`
	ServerIdentifier string `json:"serverIdentifier,omitempty"`
	SourceIP         string `json:"sourceIP" format:"ipv4"`
	GatewayIP        string `json:"gatewayIP" format:"ipv4"`
}

// DHCPTopology describes the immutable host network for a scope.
type DHCPTopology struct {
	ConflictDetection string                   `json:"conflictDetection,omitempty"`
	ProbeTimeout      string                   `json:"probeTimeout,omitempty"`
	FailoverServers   []config.DHCPScopeServer `json:"failoverServers,omitempty"`
	Interface         string                   `json:"interface"`
	ServerIP          string                   `json:"serverIP" format:"ipv4"`
	Subnet            string                   `json:"subnet"`
	RelayOnly         bool                     `json:"relayOnly"`
	TrustedRelays     []DHCPTrustedRelay       `json:"trustedRelays" nullable:"false"`
}

// DHCPScopeSummary identifies a scope and its configuration ownership.
type DHCPScopeSummary struct {
	Name       string `json:"name"`
	Management string `json:"management" enum:"file,api"`
	// ManagedBy is the writer of a Kubernetes DHCPScope that the dashboard
	// may not change, e.g. "gitops:argocd"; empty when the dashboard may.
	ManagedBy string `json:"managedBy,omitempty"`
	Revision  string `json:"revision"`
	Interface string `json:"interface"`
	ServerIP  string `json:"serverIP"`
	Subnet    string `json:"subnet"`
	RelayOnly bool   `json:"relayOnly"`
}

// DHCPStatus reports DHCP availability and scopes on the connected node.
type DHCPStatus struct {
	Failover *dhcp.ReplicationStatus `json:"failover,omitempty"`
	// Mode is node (SQLite: setup and leases belong to the connected node)
	// or kubernetes (DHCPScope and DHCPLease objects, the same on every
	// replica; one replica serves).
	Mode string `json:"mode" enum:"node,kubernetes"`
	// ServingNode is the replica that answers DHCP (kubernetes mode).
	ServingNode string `json:"servingNode,omitempty"`
	// ServerAddress is the LoadBalancer address relays send to (kubernetes mode).
	ServerAddress string `json:"serverAddress,omitempty"`
	// RuntimeError reports why scopes are not served (kubernetes mode).
	RuntimeError string `json:"runtimeError,omitempty"`
	// InvalidScopes maps scopes that are not served to the reason (kubernetes mode).
	InvalidScopes map[string]string  `json:"invalidScopes,omitempty"`
	Node          string             `json:"node"`
	Enabled       bool               `json:"enabled"`
	Available     bool               `json:"available"`
	Scopes        []DHCPScopeSummary `json:"scopes" nullable:"false"`
	Took
}

// DHCPConfig contains a scope's effective configuration and revision.
type DHCPConfig struct {
	Node         string            `json:"node"`
	Scope        string            `json:"scope"`
	Management   string            `json:"management" enum:"file,api"`
	ManagedBy    string            `json:"managedBy,omitempty"`
	Revision     string            `json:"revision"`
	ETag         string            `json:"etag"`
	Topology     DHCPTopology      `json:"topology"`
	Options      DHCPPoolOptions   `json:"options"`
	Reservations []DHCPReservation `json:"reservations" nullable:"false"`
	EnvLocked    []string          `json:"envLocked" nullable:"false"`
	Took
}

// DHCPLease describes a live lease, offer, or quarantined address.
type DHCPLease struct {
	Address   string    `json:"address" format:"ipv4"`
	ClientID  string    `json:"clientID"`
	MAC       string    `json:"mac"`
	Hostname  string    `json:"hostname"`
	State     string    `json:"state" enum:"offered,bound,declined"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// DHCPLeases is a page of live lease state for a node and scope.
type DHCPLeases struct {
	Node  string `json:"node"`
	Scope string `json:"scope"`
	List[DHCPLease]
}

// DHCPReservations contains a scope's reservations and configuration revision.
type DHCPReservations struct {
	Node     string            `json:"node"`
	Scope    string            `json:"scope"`
	Revision string            `json:"revision"`
	ETag     string            `json:"etag"`
	Items    []DHCPReservation `json:"items" nullable:"false"`
	Took
}

// DHCPImportRequest supplies a native JSON or dnsmasq lease document.
type DHCPImportRequest struct {
	Format string `json:"format" enum:"native,dnsmasq"`
	Data   string `json:"data"`
}

// DHCPImportResult reports the outcome of an atomic lease import.
type DHCPImportResult struct {
	Node           string `json:"node"`
	Scope          string `json:"scope"`
	Imported       int    `json:"imported"`
	Unchanged      int    `json:"unchanged"`
	SkippedExpired int    `json:"skippedExpired"`
	Took
}

// DHCPExport carries the native lease document without its netip Go types.
type DHCPExport struct {
	Node     string            `json:"node"`
	Scope    string            `json:"scope"`
	Document DHCPLeaseDocument `json:"document"`
	Took
}

// DHCPLeaseIdentity preserves the namespace and opaque value of a lease owner.
type DHCPLeaseIdentity struct {
	Kind  string `json:"kind" enum:"mac,client-id"`
	Value string `json:"value"`
}

// DHCPLeaseRecord preserves lease ownership, state, and absolute expiry.
type DHCPLeaseRecord struct {
	Address   string            `json:"address" format:"ipv4"`
	Identity  DHCPLeaseIdentity `json:"identity"`
	MAC       string            `json:"mac"`
	Hostname  string            `json:"hostname"`
	ExpiresAt time.Time         `json:"expiresAt"`
	State     string            `json:"state" enum:"offered,bound,declined"`
}

// DHCPLeaseDocument is the versioned native lease export format.
type DHCPLeaseDocument struct {
	Version    int               `json:"version"`
	Scope      string            `json:"scope"`
	Subnet     string            `json:"subnet"`
	ExportedAt time.Time         `json:"exportedAt"`
	Leases     []DHCPLeaseRecord `json:"leases" nullable:"false"`
}

type dhcpScopeInput struct {
	Scope string `path:"scope"`
}
type dhcpConfigOutput struct {
	ETag string `header:"ETag"`
	Body *DHCPConfig
}
type dhcpStatusOutput struct{ Body *DHCPStatus }
type dhcpReservationsOutput struct {
	ETag string `header:"ETag"`
	Body *DHCPReservations
}
type dhcpLeasesOutput struct{ Body *DHCPLeases }
type dhcpExportOutput struct{ Body *DHCPExport }
type dhcpImportOutput struct{ Body *DHCPImportResult }
type dhcpConfigInput struct {
	Scope   string `path:"scope"`
	IfMatch string `header:"If-Match" required:"true"`
	Body    DHCPPoolOptions
}
type dhcpReservationInput struct {
	Scope   string `path:"scope"`
	MAC     string `path:"mac"`
	IfMatch string `header:"If-Match" required:"true"`
	Body    struct {
		IP       string `json:"ip" format:"ipv4"`
		Hostname string `json:"hostname,omitempty"`
	}
}
type dhcpReservationDeleteInput struct {
	Scope   string `path:"scope"`
	MAC     string `path:"mac"`
	IfMatch string `header:"If-Match" required:"true"`
}
type dhcpLeasesInput struct {
	Scope    string `path:"scope"`
	State    string `query:"state" enum:"offered,bound,declined"`
	Q        string `query:"q" maxLength:"255"`
	Limit    int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
	Continue string `query:"continue"`
}
type dhcpImportInput struct {
	Scope string `path:"scope"`
	Body  DHCPImportRequest
}

func (s *Server) registerDHCP() {
	read := perm{resource: "dhcp", verb: "read"}
	s.registerDHCPSetup()
	register(s, operation(http.MethodGet, dhcpBase+"/status", "getDHCPStatus", "DHCPv4 scopes on this node", "dhcp"), read, s.dhcpStatus)
	register(s, operation(http.MethodGet, dhcpBase+"/scopes/{scope}/config", "getDHCPScopeConfig", "Effective DHCPv4 scope configuration", "dhcp"), read, s.dhcpGetConfig)
	register(s, operation(http.MethodPut, dhcpBase+"/scopes/{scope}/config", "putDHCPScopeConfig", "Atomically update DHCPv4 pool options", "dhcp"), perm{resource: "dhcp", verb: "write"}, s.dhcpPutConfig)
	register(s, operation(http.MethodGet, dhcpBase+"/scopes/{scope}/leases", "listDHCPLeases", "Live DHCPv4 leases on this node", "dhcp"), perm{resource: "dhcpleases", verb: "read"}, s.dhcpListLeases)
	register(s, operation(http.MethodGet, dhcpBase+"/scopes/{scope}/reservations", "listDHCPReservations", "Effective DHCPv4 reservations", "dhcp"), read, s.dhcpListReservations)
	register(s, operation(http.MethodPut, dhcpBase+"/scopes/{scope}/reservations/{mac}", "putDHCPReservation", "Create or replace a DHCPv4 reservation", "dhcp"), perm{resource: "dhcp", verb: "write"}, s.dhcpPutReservation)
	register(s, operation(http.MethodDelete, dhcpBase+"/scopes/{scope}/reservations/{mac}", "deleteDHCPReservation", "Remove a reservation while preserving live leases", "dhcp"), perm{resource: "dhcp", verb: "delete"}, s.dhcpDeleteReservation)
	register(s, operation(http.MethodGet, dhcpBase+"/scopes/{scope}/export", "exportDHCPLeases", "Export native DHCPv4 lease state", "dhcp"), perm{resource: "dhcpleases", verb: "export"}, s.dhcpExport)
	op := operation(http.MethodPost, dhcpBase+"/scopes/{scope}/import", "importDHCPLeases", "Atomically import native or dnsmasq leases", "dhcp")
	// A JSON string can expand each input byte into a six-byte escape.
	op.MaxBodyBytes = 6*dhcp.MaxImportBytes + 1024
	register(s, op, perm{resource: "dhcpleases", verb: "import"}, s.dhcpImport)
}

func (s *Server) dhcpScope(name string) (*dhcp.LeaseStore, error) {
	if s.opts.DHCPKube != nil {
		store, err := s.opts.DHCPKube.Store(name)
		if err != nil {
			return nil, errNotFound("DHCP scope " + name)
		}
		return store, nil
	}
	if s.opts.DHCP == nil {
		return nil, errUnavailable("DHCPv4 service is not available on this node")
	}
	if !slices.Contains(s.opts.DHCP.ScopeNames(), name) {
		return nil, errNotFound("DHCP scope " + name)
	}
	store, err := s.opts.DHCP.Scope(name)
	if err != nil {
		return nil, errUnavailable("DHCP scope: %v", err)
	}
	return store, nil
}

func dhcpManagement(value string) string {
	if value == "" {
		return "file"
	}
	return value
}
func dhcpSummary(cfg dhcp.Config, revision uint64) DHCPScopeSummary {
	return DHCPScopeSummary{Name: cfg.Name, Management: dhcpManagement(cfg.Management), Revision: strconv.FormatUint(revision, 10), Interface: cfg.Interface, ServerIP: cfg.ServerIP.String(), Subnet: cfg.Subnet.String(), RelayOnly: cfg.RelayOnly}
}

func (s *Server) dhcpStatus(ctx context.Context, _ *struct{}) (*dhcpStatusOutput, error) {
	if s.opts.DHCPKube != nil {
		return s.kubeDHCPStatus(ctx)
	}
	if s.kubeStore() {
		// dhcp.enabled is off in this release: nothing serves, and setup is a
		// Helm setting rather than this replica's.
		return &dhcpStatusOutput{Body: &DHCPStatus{Mode: dhcpKubeNode, Node: s.opts.NodeName, Scopes: []DHCPScopeSummary{}}}, nil
	}
	out := &DHCPStatus{Mode: "node", Node: s.opts.NodeName, Enabled: s.opts.DHCP != nil && s.config().DHCP.Enabled, Available: s.opts.DHCP != nil, Scopes: []DHCPScopeSummary{}}
	if s.opts.DHCP != nil {
		status := s.opts.DHCP.Replication()
		if status.Enabled {
			out.Failover = &status
		}
		for _, name := range s.opts.DHCP.ScopeNames() {
			store, err := s.opts.DHCP.Scope(name)
			if err != nil {
				return nil, errUnavailable("DHCP scope: %v", err)
			}
			state := store.Configuration()
			out.Scopes = append(out.Scopes, dhcpSummary(state.Config, state.Revision))
		}
	} else if configs, err := s.config().DHCP.Configs(); err == nil {
		for _, cfg := range configs {
			out.Scopes = append(out.Scopes, dhcpSummary(cfg, 0))
		}
	} else {
		for _, cfg := range s.config().DHCP.Scopes {
			out.Scopes = append(out.Scopes, DHCPScopeSummary{Name: cfg.Name, Management: dhcpManagement(cfg.Management), Revision: "0", Interface: cfg.Interface, ServerIP: cfg.ServerIP, Subnet: cfg.Subnet, RelayOnly: cfg.RelayOnly})
		}
	}
	return &dhcpStatusOutput{Body: out}, nil
}

func dhcpOptions(cfg dhcp.Config) DHCPPoolOptions {
	extra := config.DHCPScopeFromConfig(cfg)
	dns := make([]string, 0, len(cfg.DNSServers))
	for _, ip := range cfg.DNSServers {
		dns = append(dns, ip.String())
	}
	return DHCPPoolOptions{NTPServers: extra.NTPServers, DomainSearch: extra.DomainSearch, ClasslessRoutes: extra.ClasslessRoutes, BootServer: extra.BootServer, BootServerName: extra.BootServerName, BootFile: extra.BootFile, BootProfiles: extra.BootProfiles, RangeStart: cfg.RangeStart.String(), RangeEnd: cfg.RangeEnd.String(), Router: cfg.Router.String(), DNSServers: dns, DomainName: cfg.DomainName, LeaseDuration: cfg.LeaseDuration.String(), RegisterDNS: cfg.RegisterDNS, DNSTTL: cfg.DNSTTL.String()}
}

func dhcpETag(node, scope string, revision uint64) string {
	raw, _ := json.Marshal([]string{node, scope, strconv.FormatUint(revision, 10)})
	return `"` + base64.RawURLEncoding.EncodeToString(raw) + `"`
}

func (s *Server) dhcpLocks(_ string) []string {
	locked := []string{}
	fields := []string{"rangeStart", "rangeEnd", "router", "dnsServers", "domainName", "leaseDuration", "registerDNS", "dnsTTL", "reservations", "ntpServers", "domainSearch", "classlessRoutes", "bootServer", "bootServerName", "bootFile", "bootProfiles"}
	cfg := s.config()
	for _, field := range fields {
		if len(cfg.DHCP.Scopes) > 0 && slices.Contains(cfg.EnvLocked, "dhcp.scopes") || len(cfg.DHCP.Scopes) == 0 && slices.Contains(cfg.EnvLocked, "dhcp."+field) {
			locked = append(locked, field)
		}
	}
	return locked
}

func (s *Server) dhcpConfig(name string, state dhcp.ManagementState) *dhcpConfigOutput {
	cfg := state.Config
	reservations := make([]DHCPReservation, 0, len(cfg.Reservations))
	for _, r := range cfg.Reservations {
		reservations = append(reservations, DHCPReservation{MAC: r.MAC, IP: r.IP.String(), Hostname: r.Hostname})
	}
	relays := make([]DHCPTrustedRelay, 0, len(cfg.TrustedRelays))
	extra := config.DHCPScopeFromConfig(cfg)
	for _, r := range extra.TrustedRelays {
		relays = append(relays, DHCPTrustedRelay{SourceIP: r.SourceIP, GatewayIP: r.GatewayIP, SourcePort: r.SourcePort, LinkSelection: r.LinkSelection, ServerIdentifier: r.ServerIdentifier})
	}
	tag := dhcpETag(s.opts.NodeName, name, state.Revision)
	return &dhcpConfigOutput{ETag: tag, Body: &DHCPConfig{Node: s.opts.NodeName, Scope: name, Management: dhcpManagement(cfg.Management), Revision: strconv.FormatUint(state.Revision, 10), ETag: tag, Topology: DHCPTopology{ConflictDetection: cfg.ConflictDetection, ProbeTimeout: cfg.ProbeTimeout.String(), FailoverServers: extra.FailoverServers, Interface: cfg.Interface, ServerIP: cfg.ServerIP.String(), Subnet: cfg.Subnet.String(), RelayOnly: cfg.RelayOnly, TrustedRelays: relays}, Options: dhcpOptions(cfg), Reservations: reservations, EnvLocked: s.dhcpLocks(name)}}
}

func (s *Server) dhcpGetConfig(ctx context.Context, in *dhcpScopeInput) (*dhcpConfigOutput, error) {
	if s.opts.DHCPKube != nil {
		scope, err := s.kubeDHCPScope(ctx, in.Scope)
		if err != nil {
			return nil, err
		}
		return s.kubeDHCPConfig(scope), nil
	}
	store, err := s.dhcpScope(in.Scope)
	if err != nil {
		return nil, err
	}
	return s.dhcpConfig(in.Scope, store.Configuration()), nil
}

func (s *Server) dhcpMutate(ctx context.Context, name, tag, action string, fn func(context.Context, dhcp.ManagementState) error) (*dhcpConfigOutput, error) {
	if _, err := s.dhcpScope(name); err != nil {
		return nil, err
	}
	store, err := s.dhcpScope(name)
	if err != nil {
		return nil, err
	}
	before := store.Configuration()
	after := before
	apply := func() error {
		if tag == "" {
			return errInvalid("If-Match: required")
		}
		current := dhcpETag(s.opts.NodeName, name, before.Revision)
		if tag != current {
			return errPrecondition(tag, current)
		}
		if err := fn(ctx, before); err != nil {
			return err
		}
		after = store.Configuration()
		return nil
	}
	err = apply()
	s.recordAction(ctx, action, audit.Resource{Kind: "dhcp", Name: name}, err, s.dhcpConfig(name, before).Body, s.dhcpConfig(name, after).Body)
	if err != nil {
		return nil, dhcpError(err)
	}
	return s.dhcpConfig(name, after), nil
}

func dhcpError(err error) error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	switch {
	case errors.Is(err, dhcp.ErrRevisionConflict):
		return newError(http.StatusPreconditionFailed, KeyPrecondition, "DHCP scope revision changed", "reload the scope configuration")
	case errors.Is(err, dhcp.ErrManagedByFile):
		return newError(http.StatusConflict, KeyManagedBy, "DHCP scope is managed by the host file", "set management: api on the host and restart to delegate pool options and reservations")
	case errors.Is(err, dhcp.ErrUnavailable):
		return errConflict("DHCP change conflicts with a live lease or reservation", "wait for existing leases or quarantines to expire; do not delete lease state")
	case errors.Is(err, dhcp.ErrInvalidImport):
		return errInvalid("DHCP import: %v", err)
	case errors.Is(err, dhcp.ErrImportTooLarge):
		return newError(http.StatusRequestEntityTooLarge, KeyTooLarge, "DHCP import exceeds 4 MiB", "split the input into smaller atomic imports")
	default:
		return errUnavailable("DHCP storage: %v", err)
	}
}

func (s *Server) dhcpPutConfig(ctx context.Context, in *dhcpConfigInput) (*dhcpConfigOutput, error) {
	if s.opts.DHCPKube != nil {
		return s.kubeDHCPWrite(ctx, in.Scope, in.IfMatch, "dhcp.write", func(scope *v1alpha1.DHCPScope) error {
			return kubeApplyOptions(&scope.Spec, in.Body)
		})
	}
	return s.dhcpMutate(ctx, in.Scope, in.IfMatch, "dhcp.write", func(ctx context.Context, state dhcp.ManagementState) error {
		if dhcpManagement(state.Config.Management) != "api" {
			return dhcp.ErrManagedByFile
		}
		old, _ := json.Marshal(dhcpOptions(state.Config))
		next, _ := json.Marshal(in.Body)
		var oldFields, newFields map[string]any
		_ = json.Unmarshal(old, &oldFields)
		_ = json.Unmarshal(next, &newFields)
		for _, field := range s.dhcpLocks(in.Scope) {
			if field != "reservations" && !reflect.DeepEqual(oldFields[field], newFields[field]) {
				return newError(http.StatusConflict, KeyEnvLocked, "dhcp."+field+" is set by the environment", "change the host environment")
			}
		}
		opts, err := in.Body.runtime()
		if err != nil {
			return err
		}
		candidate := state.Config
		candidate.RangeStart, candidate.RangeEnd, candidate.Router = opts.RangeStart, opts.RangeEnd, opts.Router
		candidate.DNSServers, candidate.DomainName, candidate.LeaseDuration = opts.DNSServers, opts.DomainName, opts.LeaseDuration
		candidate.RegisterDNS, candidate.DNSTTL = opts.RegisterDNS, opts.DNSTTL
		candidate.NTPServers = opts.NTPServers
		candidate.DomainSearch = opts.DomainSearch
		candidate.ClasslessRoutes = opts.ClasslessRoutes
		candidate.BootServer = opts.BootServer
		candidate.BootServerName = opts.BootServerName
		candidate.BootFile = opts.BootFile
		candidate.BootProfiles = opts.BootProfiles

		if err := candidate.Validate(); err != nil {
			return errInvalid("DHCP options: %v", err)
		}
		return s.opts.DHCP.UpdateOptions(ctx, in.Scope, state.Revision, opts)
	})
}

func (o DHCPPoolOptions) runtime() (dhcp.PoolOptions, error) {
	var out dhcp.PoolOptions
	var err error
	for _, field := range []struct {
		name, value string
		target      *netip.Addr
	}{{"rangeStart", o.RangeStart, &out.RangeStart}, {"rangeEnd", o.RangeEnd, &out.RangeEnd}, {"router", o.Router, &out.Router}} {
		*field.target, err = netip.ParseAddr(field.value)
		if err != nil || !field.target.Is4() {
			return out, errInvalid("%s: an IPv4 address is required", field.name)
		}
	}
	for i, value := range o.DNSServers {
		ip, err := netip.ParseAddr(value)
		if err != nil || !ip.Is4() {
			return out, errInvalid("dnsServers[%d]: an IPv4 address is required", i)
		}
		out.DNSServers = append(out.DNSServers, ip)
	}
	out.LeaseDuration, err = time.ParseDuration(o.LeaseDuration)
	if err != nil {
		return out, errInvalid("leaseDuration: %v", err)
	}
	out.DNSTTL, err = time.ParseDuration(o.DNSTTL)
	if err != nil {
		return out, errInvalid("dnsTTL: %v", err)
	}
	if out.DNSTTL < time.Second || out.DNSTTL > time.Hour || out.DNSTTL%time.Second != 0 {
		return out, errInvalid("dnsTTL: use whole seconds from 1s through 1h")
	}
	out.DomainName, out.RegisterDNS = o.DomainName, o.RegisterDNS
	extra := config.DHCPScope{NTPServers: o.NTPServers, DomainSearch: o.DomainSearch, ClasslessRoutes: o.ClasslessRoutes, BootServer: o.BootServer, BootServerName: o.BootServerName, BootFile: o.BootFile, BootProfiles: o.BootProfiles}
	var wire dhcp.Config
	if err := extra.ApplyExtendedOptions(&wire); err != nil {
		return out, errInvalid("DHCP options: %v", err)
	}
	out.NTPServers = wire.NTPServers
	out.DomainSearch = wire.DomainSearch
	out.ClasslessRoutes = wire.ClasslessRoutes
	out.BootServer = wire.BootServer
	out.BootServerName = wire.BootServerName
	out.BootFile = wire.BootFile
	out.BootProfiles = wire.BootProfiles

	return out, nil
}

func dhcpMAC(value string) (string, error) {
	mac, err := net.ParseMAC(value)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
		return "", errInvalid("mac: a nonzero unicast six-octet Ethernet address is required")
	}
	return mac.String(), nil
}

func (s *Server) dhcpListReservations(ctx context.Context, in *dhcpScopeInput) (*dhcpReservationsOutput, error) {
	if s.opts.DHCPKube != nil {
		scope, err := s.kubeDHCPScope(ctx, in.Scope)
		if err != nil {
			return nil, err
		}
		cfg := s.kubeDHCPConfig(scope)
		return &dhcpReservationsOutput{ETag: cfg.ETag, Body: &DHCPReservations{Node: cfg.Body.Node, Scope: in.Scope, Revision: cfg.Body.Revision, ETag: cfg.ETag, Items: cfg.Body.Reservations}}, nil
	}
	store, err := s.dhcpScope(in.Scope)
	if err != nil {
		return nil, err
	}
	cfg := s.dhcpConfig(in.Scope, store.Configuration())
	return &dhcpReservationsOutput{ETag: cfg.ETag, Body: &DHCPReservations{Node: cfg.Body.Node, Scope: in.Scope, Revision: cfg.Body.Revision, ETag: cfg.ETag, Items: cfg.Body.Reservations}}, nil
}

func (s *Server) dhcpPutReservation(ctx context.Context, in *dhcpReservationInput) (*dhcpConfigOutput, error) {
	if s.opts.DHCPKube != nil {
		return s.kubeDHCPWrite(ctx, in.Scope, in.IfMatch, "dhcp.write", func(scope *v1alpha1.DHCPScope) error {
			mac, err := dhcpMAC(in.MAC)
			if err != nil {
				return err
			}
			reservation := v1alpha1.DHCPReservation{MAC: mac, IP: in.Body.IP, Hostname: in.Body.Hostname}
			for i, r := range scope.Spec.Reservations {
				if canonical, err := dhcpMAC(r.MAC); err == nil && canonical == mac {
					scope.Spec.Reservations[i] = reservation
					return nil
				}
			}
			scope.Spec.Reservations = append(scope.Spec.Reservations, reservation)
			return nil
		})
	}
	return s.dhcpMutate(ctx, in.Scope, in.IfMatch, "dhcp.write", func(ctx context.Context, state dhcp.ManagementState) error {
		if dhcpManagement(state.Config.Management) != "api" {
			return dhcp.ErrManagedByFile
		}
		if slices.Contains(s.dhcpLocks(in.Scope), "reservations") {
			return newError(http.StatusConflict, KeyEnvLocked, "DHCP reservations are set by the environment", "change the host environment")
		}
		mac, err := dhcpMAC(in.MAC)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(in.Body.IP)
		if err != nil || !ip.Is4() {
			return errInvalid("ip: an IPv4 address is required")
		}
		reservation := dhcp.Reservation{MAC: mac, IP: ip, Hostname: in.Body.Hostname}
		candidate := state.Config
		candidate.Reservations = slices.DeleteFunc(slices.Clone(candidate.Reservations), func(r dhcp.Reservation) bool { return r.MAC == mac })
		candidate.Reservations = append(candidate.Reservations, reservation)
		if err := candidate.Validate(); err != nil {
			return errInvalid("reservation: %v", err)
		}
		return s.opts.DHCP.PutReservation(ctx, in.Scope, state.Revision, reservation)
	})
}

func (s *Server) dhcpDeleteReservation(ctx context.Context, in *dhcpReservationDeleteInput) (*dhcpConfigOutput, error) {
	if s.opts.DHCPKube != nil {
		return s.kubeDHCPWrite(ctx, in.Scope, in.IfMatch, "dhcp.delete", func(scope *v1alpha1.DHCPScope) error {
			mac, err := dhcpMAC(in.MAC)
			if err != nil {
				return err
			}
			scope.Spec.Reservations = slices.DeleteFunc(scope.Spec.Reservations, func(r v1alpha1.DHCPReservation) bool {
				canonical, err := dhcpMAC(r.MAC)
				return err == nil && canonical == mac
			})
			return nil
		})
	}
	return s.dhcpMutate(ctx, in.Scope, in.IfMatch, "dhcp.delete", func(ctx context.Context, state dhcp.ManagementState) error {
		if dhcpManagement(state.Config.Management) != "api" {
			return dhcp.ErrManagedByFile
		}
		if slices.Contains(s.dhcpLocks(in.Scope), "reservations") {
			return newError(http.StatusConflict, KeyEnvLocked, "DHCP reservations are set by the environment", "change the host environment")
		}
		mac, err := dhcpMAC(in.MAC)
		if err != nil {
			return err
		}
		return s.opts.DHCP.DeleteReservation(ctx, in.Scope, state.Revision, mac)
	})
}

func (s *Server) dhcpListLeases(ctx context.Context, in *dhcpLeasesInput) (*dhcpLeasesOutput, error) {
	store, err := s.dhcpScope(in.Scope)
	if err != nil {
		return nil, err
	}
	after := netip.Addr{}
	if in.Continue != "" {
		raw, err := decodeCursor(in.Continue)
		if err != nil {
			return nil, err
		}
		var cursor struct{ Node, Scope, State, Q, Address string }
		if json.Unmarshal([]byte(raw), &cursor) != nil || cursor.Node != s.dhcpCursorNode() || cursor.Scope != in.Scope || cursor.State != in.State || cursor.Q != in.Q {
			return nil, errInvalid("continue: not a token for this DHCP lease query")
		}
		after, err = netip.ParseAddr(cursor.Address)
		if err != nil || !after.Is4() {
			return nil, errInvalid("continue: invalid DHCP lease position")
		}
	}
	leases, err := store.List(ctx)
	if err != nil {
		return nil, dhcpError(err)
	}
	items := []DHCPLease{}
	next := ""
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	q := strings.ToLower(in.Q)
	for _, lease := range leases {
		if after.IsValid() && lease.Address.Compare(after) <= 0 || in.State != "" && lease.State != in.State {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(lease.Address.String()+" "+lease.ClientID+" "+lease.MAC+" "+lease.Hostname), q) {
			continue
		}
		if len(items) == limit {
			raw, _ := json.Marshal(struct{ Node, Scope, State, Q, Address string }{s.dhcpCursorNode(), in.Scope, in.State, in.Q, items[len(items)-1].Address})
			next = encodeCursor(string(raw))
			break
		}
		items = append(items, DHCPLease{Address: lease.Address.String(), ClientID: lease.ClientID, MAC: lease.MAC, Hostname: lease.Hostname, State: lease.State, ExpiresAt: lease.ExpiresAt})
	}
	return &dhcpLeasesOutput{Body: &DHCPLeases{Node: s.opts.NodeName, Scope: in.Scope, List: *newList(items, next)}}, nil
}

func (s *Server) dhcpExport(ctx context.Context, in *dhcpScopeInput) (*dhcpExportOutput, error) {
	store, err := s.dhcpScope(in.Scope)
	if err != nil {
		return nil, err
	}
	doc, err := store.Export(ctx)
	if err != nil {
		return nil, dhcpError(err)
	}
	out := DHCPLeaseDocument{Version: doc.Version, Scope: doc.Scope, Subnet: doc.Subnet.String(), ExportedAt: doc.ExportedAt, Leases: []DHCPLeaseRecord{}}
	for _, l := range doc.Leases {
		out.Leases = append(out.Leases, DHCPLeaseRecord{Address: l.Address.String(), Identity: DHCPLeaseIdentity{Kind: l.Identity.Kind, Value: l.Identity.Value}, MAC: l.MAC, Hostname: l.Hostname, ExpiresAt: l.ExpiresAt, State: l.State})
	}
	return &dhcpExportOutput{Body: &DHCPExport{Node: s.opts.NodeName, Scope: in.Scope, Document: out}}, nil
}

func (s *Server) dhcpImport(ctx context.Context, in *dhcpImportInput) (*dhcpImportOutput, error) {
	if _, err := s.dhcpScope(in.Scope); err != nil {
		return nil, err
	}
	if len(in.Body.Data) > dhcp.MaxImportBytes {
		return nil, dhcpError(dhcp.ErrImportTooLarge)
	}
	var result dhcp.ImportResult
	var err error
	if s.opts.DHCPKube != nil {
		result, err = s.opts.DHCPKube.Import(ctx, in.Scope, in.Body.Format, []byte(in.Body.Data))
	} else {
		result, err = s.opts.DHCP.Import(ctx, in.Scope, in.Body.Format, []byte(in.Body.Data))
	}
	s.recordAction(ctx, "dhcpleases.import", audit.Resource{Kind: "dhcpleases", Name: in.Scope}, err, nil, result)
	if err != nil {
		return nil, dhcpError(err)
	}
	return &dhcpImportOutput{Body: &DHCPImportResult{Node: s.opts.NodeName, Scope: in.Scope, Imported: result.Imported, Unchanged: result.Unchanged, SkippedExpired: result.SkippedExpired}}, nil
}
