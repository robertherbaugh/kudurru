package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcpsetup"
)

// DHCPSetupScope is a complete desired scope, activated only after a restart.
type DHCPSetupScope struct {
	NTPServers        []string                    `json:"ntpServers,omitempty"`
	DomainSearch      []string                    `json:"domainSearch,omitempty"`
	ClasslessRoutes   []config.DHCPClasslessRoute `json:"classlessRoutes,omitempty"`
	BootServer        string                      `json:"bootServer,omitempty"`
	BootServerName    string                      `json:"bootServerName,omitempty"`
	BootFile          string                      `json:"bootFile,omitempty"`
	BootProfiles      []config.DHCPBootProfile    `json:"bootProfiles,omitempty"`
	ConflictDetection string                      `json:"conflictDetection,omitempty"`
	ProbeTimeout      string                      `json:"probeTimeout,omitempty"`
	FailoverServers   []config.DHCPScopeServer    `json:"failoverServers,omitempty"`
	Name              string                      `json:"name" minLength:"1" maxLength:"63"`
	Interface         string                      `json:"interface"`
	ServerIP          string                      `json:"serverIP" format:"ipv4"`
	Subnet            string                      `json:"subnet"`
	RelayOnly         bool                        `json:"relayOnly"`
	TrustedRelays     []DHCPTrustedRelay          `json:"trustedRelays" nullable:"false"`
	RangeStart        string                      `json:"rangeStart" format:"ipv4"`
	RangeEnd          string                      `json:"rangeEnd" format:"ipv4"`
	Router            string                      `json:"router" format:"ipv4"`
	DNSServers        []string                    `json:"dnsServers" nullable:"false" minItems:"1" maxItems:"63"`
	DomainName        string                      `json:"domainName"`
	LeaseDuration     string                      `json:"leaseDuration"`
	RegisterDNS       bool                        `json:"registerDNS"`
	DNSTTL            string                      `json:"dnsTTL"`
	Reservations      []DHCPReservation           `json:"reservations" nullable:"false"`
}

// DHCPSetup reports saved desired setup separately from the running service.
type DHCPSetup struct {
	Failover            config.DHCPFailover `json:"failover"`
	FailoverInitialized bool                `json:"failoverInitialized"`
	// Mode is node (setup for the connected node, applied at restart) or
	// kubernetes (DHCPScope objects, applied live on every replica).
	Mode string `json:"mode" enum:"node,kubernetes"`
	// ScopeOwners maps Kubernetes DHCPScopes the dashboard may not change to
	// their writer, e.g. "gitops:argocd".
	ScopeOwners map[string]string `json:"scopeOwners,omitempty"`
	// ServerAddress is the LoadBalancer address relays send to (kubernetes mode).
	ServerAddress     string            `json:"serverAddress,omitempty"`
	Node              string            `json:"node"`
	Enabled           bool              `json:"enabled"`
	ManagedBy         string            `json:"managedBy" enum:"file,api"`
	EnvironmentLocked bool              `json:"environmentLocked"`
	RestartRequired   bool              `json:"restartRequired"`
	Available         bool              `json:"available"`
	UnavailableReason string            `json:"unavailableReason"`
	RuntimeError      string            `json:"runtimeError"`
	Revision          string            `json:"revision"`
	ETag              string            `json:"etag"`
	Scopes            []DHCPSetupScope  `json:"scopes" nullable:"false"`
	ScopeRevisions    map[string]string `json:"scopeRevisions" nullable:"false"`
	PendingScopes     []string          `json:"pendingScopes" nullable:"false"`
	Took
}

// DHCPSetupRequest replaces the complete desired registry without live activation.
type DHCPSetupRequest struct {
	Failover       *config.DHCPFailover `json:"failover,omitempty"`
	Enabled        bool                 `json:"enabled"`
	Scopes         []DHCPSetupScope     `json:"scopes" nullable:"false" maxItems:"256"`
	Adopt          bool                 `json:"adopt,omitempty"`
	ScopeRevisions map[string]string    `json:"scopeRevisions" nullable:"false"`
}

// DHCPInterface describes a host interface without sending network packets.
type DHCPInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses" nullable:"false"`
	Up        bool     `json:"up"`
	Broadcast bool     `json:"broadcast"`
	Loopback  bool     `json:"loopback"`
}

// DHCPInterfaces is the node-local IPv4 interface inventory.
type DHCPInterfaces struct {
	Node  string          `json:"node"`
	Items []DHCPInterface `json:"items" nullable:"false"`
	Took
}

type dhcpSetupInput struct {
	IfMatch string `header:"If-Match" required:"true"`
	Body    DHCPSetupRequest
}
type dhcpSetupOutput struct {
	ETag string `header:"ETag"`
	Body *DHCPSetup
}
type dhcpInterfacesOutput struct{ Body *DHCPInterfaces }

func (s *Server) registerDHCPSetup() {
	read := perm{resource: "dhcp", verb: "read"}
	register(s, operation(http.MethodGet, dhcpBase+"/setup", "getDHCPSetup", "Saved DHCPv4 setup on this node", "dhcp"), read, s.dhcpGetSetup)
	op := operation(http.MethodPut, dhcpBase+"/setup", "putDHCPSetup", "Save DHCPv4 setup for the next restart", "dhcp")
	op.Description = "Persists desired setup without changing running listeners or lease stores. Every saved scope must be complete, even when DHCP is disabled. File ownership requires explicit adoption."
	register(s, op, perm{resource: "dhcp", verb: "write"}, s.dhcpPutSetup)
	register(s, operation(http.MethodGet, dhcpBase+"/interfaces", "listDHCPInterfaces", "IPv4 interfaces on this node", "dhcp"), read, s.dhcpListInterfaces)
}

func dhcpSetupETag(node string, revision uint64) string {
	// A separate namespace prevents a lease-scope ETag from authorizing setup.
	raw, _ := json.Marshal([]string{node, "dhcp", "setup", strconv.FormatUint(revision, 10)})
	return `"` + base64.RawURLEncoding.EncodeToString(raw) + `"`
}

func (s *Server) dhcpSetupResponse(state dhcpsetup.State) *dhcpSetupOutput {
	tag := dhcpSetupETag(s.opts.NodeName, state.Revision)
	out := &DHCPSetup{Mode: "node", FailoverInitialized: state.FailoverInitialized, Failover: state.Failover.Clone(), Node: s.opts.NodeName, Enabled: state.Enabled, ManagedBy: state.ManagedBy,
		EnvironmentLocked: state.EnvironmentLocked, RestartRequired: state.RestartRequired || s.opts.DHCPSetupError != "",
		Available: s.opts.DHCPSetup != nil, RuntimeError: s.opts.DHCPSetupError,
		Revision: strconv.FormatUint(state.Revision, 10), ETag: tag, Scopes: []DHCPSetupScope{},
		ScopeRevisions: map[string]string{}, PendingScopes: append([]string{}, state.PendingScopes...)}
	for name, revision := range state.ScopeRevisions {
		out.ScopeRevisions[name] = strconv.FormatUint(revision, 10)
	}
	if !out.Available {
		out.UnavailableReason = "DHCP setup manager is not available on this node"
	}
	for _, scope := range state.Scopes {
		out.Scopes = append(out.Scopes, dhcpSetupScope(scope))
	}
	return &dhcpSetupOutput{ETag: tag, Body: out}
}

func dhcpSetupScope(scope config.DHCPScope) DHCPSetupScope {
	out := DHCPSetupScope{NTPServers: scope.NTPServers, DomainSearch: scope.DomainSearch, ClasslessRoutes: scope.ClasslessRoutes, BootServer: scope.BootServer, BootServerName: scope.BootServerName, BootFile: scope.BootFile, BootProfiles: scope.BootProfiles, ConflictDetection: scope.ConflictDetection, ProbeTimeout: dhcpOptionalDuration(scope.ProbeTimeout.Std()), FailoverServers: append([]config.DHCPScopeServer(nil), scope.FailoverServers...), Name: scope.Name, Interface: scope.Interface, ServerIP: scope.ServerIP,
		Subnet: scope.Subnet, RelayOnly: scope.RelayOnly, TrustedRelays: []DHCPTrustedRelay{},
		RangeStart: scope.RangeStart, RangeEnd: scope.RangeEnd, Router: scope.Router,
		DNSServers: append([]string{}, scope.DNSServers...), DomainName: scope.DomainName,
		LeaseDuration: scope.LeaseDuration.Std().String(), RegisterDNS: scope.RegisterDNS,
		DNSTTL: scope.DNSTTL.Std().String(), Reservations: []DHCPReservation{}}
	for _, relay := range scope.TrustedRelays {
		out.TrustedRelays = append(out.TrustedRelays, DHCPTrustedRelay{SourceIP: relay.SourceIP, GatewayIP: relay.GatewayIP, SourcePort: relay.SourcePort, LinkSelection: relay.LinkSelection, ServerIdentifier: relay.ServerIdentifier})
	}
	for _, reservation := range scope.Reservations {
		out.Reservations = append(out.Reservations, DHCPReservation{MAC: reservation.MAC, IP: reservation.IP, Hostname: reservation.Hostname})
	}
	return out
}

func (scope DHCPSetupScope) config() (config.DHCPScope, error) {
	lease, err := time.ParseDuration(scope.LeaseDuration)
	if err != nil {
		return config.DHCPScope{}, errInvalid("scope %q leaseDuration: %v", scope.Name, err)
	}
	ttl, err := time.ParseDuration(scope.DNSTTL)
	if err != nil {
		return config.DHCPScope{}, errInvalid("scope %q dnsTTL: %v", scope.Name, err)
	}
	// Zero would otherwise be interpreted as a seed default by Configs.
	if lease <= 0 || ttl < time.Second || ttl > time.Hour || ttl%time.Second != 0 {
		return config.DHCPScope{}, errInvalid("scope %q: leaseDuration must be positive; dnsTTL must use whole seconds from 1s through 1h", scope.Name)
	}
	probeTimeout := time.Duration(0)
	if scope.ProbeTimeout != "" {
		probeTimeout, err = time.ParseDuration(scope.ProbeTimeout)
		if err != nil {
			return config.DHCPScope{}, errInvalid("probeTimeout: %v", err)
		}
	}
	out := config.DHCPScope{NTPServers: scope.NTPServers, DomainSearch: scope.DomainSearch, ClasslessRoutes: scope.ClasslessRoutes, BootServer: scope.BootServer, BootServerName: scope.BootServerName, BootFile: scope.BootFile, BootProfiles: scope.BootProfiles, ConflictDetection: scope.ConflictDetection, ProbeTimeout: config.Duration(probeTimeout), FailoverServers: append([]config.DHCPScopeServer(nil), scope.FailoverServers...), Name: scope.Name, Management: "api", Interface: scope.Interface,
		ServerIP: scope.ServerIP, Subnet: scope.Subnet, RelayOnly: scope.RelayOnly,
		RangeStart: scope.RangeStart, RangeEnd: scope.RangeEnd, Router: scope.Router,
		DNSServers: append([]string{}, scope.DNSServers...), DomainName: scope.DomainName,
		LeaseDuration: config.Duration(lease), RegisterDNS: scope.RegisterDNS, DNSTTL: config.Duration(ttl)}
	for _, relay := range scope.TrustedRelays {
		out.TrustedRelays = append(out.TrustedRelays, config.DHCPTrustedRelay{SourceIP: relay.SourceIP, GatewayIP: relay.GatewayIP, SourcePort: relay.SourcePort, LinkSelection: relay.LinkSelection, ServerIdentifier: relay.ServerIdentifier})
	}
	for _, reservation := range scope.Reservations {
		out.Reservations = append(out.Reservations, config.DHCPReservation{MAC: reservation.MAC, IP: reservation.IP, Hostname: reservation.Hostname})
	}
	return out, nil
}

func (s *Server) dhcpGetSetup(ctx context.Context, _ *struct{}) (*dhcpSetupOutput, error) {
	if s.opts.DHCPKube != nil {
		return s.kubeDHCPSetup(ctx)
	}
	if s.kubeStore() {
		out := s.dhcpSetupResponse(dhcpsetup.State{ManagedBy: "api", Revision: 1})
		out.Body.Mode = dhcpKubeNode
		out.Body.UnavailableReason = "DHCP is off in this release: set dhcp.enabled and dhcp.serverAddress in the Helm values"
		return out, nil
	}
	state := dhcpsetup.State{ManagedBy: "api", Revision: 1}
	if s.opts.DHCPSetup != nil {
		var err error
		state, err = s.opts.DHCPSetup.Read(ctx)
		if err != nil {
			return nil, dhcpSetupError(err)
		}
	}
	return s.dhcpSetupResponse(state), nil
}

func (s *Server) dhcpPutSetup(ctx context.Context, in *dhcpSetupInput) (*dhcpSetupOutput, error) {
	if s.opts.DHCPKube != nil {
		return s.kubeDHCPPutSetup(ctx, in)
	}
	manager := s.opts.DHCPSetup
	if manager == nil {
		return nil, errUnavailable("DHCP setup manager is not available on this node")
	}
	before, err := manager.Read(ctx)
	if err != nil {
		return nil, dhcpSetupError(err)
	}
	after := before
	apply := func() error {
		current := dhcpSetupETag(s.opts.NodeName, before.Revision)
		if in.IfMatch != current {
			return errPrecondition(in.IfMatch, current)
		}
		scopes := make([]config.DHCPScope, 0, len(in.Body.Scopes))
		for _, scope := range in.Body.Scopes {
			converted, err := scope.config()
			if err != nil {
				return err
			}
			scopes = append(scopes, converted)
		}
		expected := make(map[string]uint64, len(in.Body.ScopeRevisions))
		for name, value := range in.Body.ScopeRevisions {
			revision, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return errInvalid("scopeRevisions[%q]: an unsigned decimal revision is required", name)
			}
			expected[name] = revision
		}
		failover := before.Failover
		if in.Body.Failover != nil {
			failover = in.Body.Failover.Clone()
		}
		saved, err := manager.Save(ctx, before.Revision, in.Body.Enabled, scopes, in.Body.Adopt, expected, failover)
		if err == nil {
			after = saved
		}
		return err
	}
	err = apply()
	s.recordAction(ctx, "dhcp.setup", audit.Resource{Kind: "dhcp", Name: "setup"}, err, s.dhcpSetupResponse(before).Body, s.dhcpSetupResponse(after).Body)
	if err != nil {
		return nil, dhcpSetupError(err)
	}
	return s.dhcpSetupResponse(after), nil
}

func dhcpSetupError(err error) error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	switch {
	case errors.Is(err, dhcpsetup.ErrRevisionConflict):
		return newError(http.StatusPreconditionFailed, KeyPrecondition, "DHCP setup revision changed", "reload setup and retry with its ETag")
	case errors.Is(err, dhcpsetup.ErrManagedByFile):
		return newError(http.StatusConflict, KeyManagedBy, "DHCP setup is managed by the host file", "explicitly adopt the setup to manage it through this node's DHCP API")
	case errors.Is(err, dhcpsetup.ErrEnvironmentLocked):
		return newError(http.StatusConflict, KeyEnvLocked, err.Error(), "remove DHCP environment overrides before using UI setup")
	case errors.Is(err, dhcpsetup.ErrInvalidConfiguration):
		return errInvalid("DHCP setup: %v", err)
	default:
		return errUnavailable("DHCP setup storage: %v", err)
	}
}

func (s *Server) dhcpListInterfaces(_ context.Context, _ *struct{}) (*dhcpInterfacesOutput, error) {
	if s.opts.DHCPKube != nil {
		// The pod interface is a node setting (dhcp.interface); pods have no
		// LAN interfaces to choose from.
		return &dhcpInterfacesOutput{Body: &DHCPInterfaces{Node: s.opts.NodeName, Items: []DHCPInterface{}}}, nil
	}
	items, err := discoverDHCPInterfaces(net.Interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
	if err != nil {
		return nil, errUnavailable("DHCP interface inventory: %v", err)
	}
	return &dhcpInterfacesOutput{Body: &DHCPInterfaces{Node: s.opts.NodeName, Items: items}}, nil
}

func discoverDHCPInterfaces(list func() ([]net.Interface, error), addresses func(net.Interface) ([]net.Addr, error)) ([]DHCPInterface, error) {
	interfaces, err := list()
	if err != nil {
		return nil, err
	}
	items := make([]DHCPInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		addrs, err := addresses(iface)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", iface.Name, err)
		}
		item := DHCPInterface{Name: iface.Name, Addresses: []string{}, Up: iface.Flags&net.FlagUp != 0,
			Broadcast: iface.Flags&net.FlagBroadcast != 0, Loopback: iface.Flags&net.FlagLoopback != 0}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			ip := prefix.Addr()
			if err != nil {
				ip, _ = netip.ParseAddr(addr.String())
			}
			if ip = ip.Unmap(); ip.Is4() {
				item.Addresses = append(item.Addresses, ip.String())
			}
		}
		slices.Sort(item.Addresses)
		item.Addresses = slices.Compact(item.Addresses)
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b DHCPInterface) int { return strings.Compare(a.Name, b.Name) })
	return items, nil
}

func dhcpOptionalDuration(v time.Duration) string {
	if v == 0 {
		return ""
	}
	return v.String()
}
