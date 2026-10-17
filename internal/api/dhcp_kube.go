package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpkube"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DHCPCluster is the DHCP service of the kube store (internal/dhcpkube):
// scopes are DHCPScope objects, leases DHCPLease objects, and every replica
// keeps the same working copy, so these routes answer the same on any
// replica a load balancer picks.
type DHCPCluster interface {
	Status() dhcpkube.Status
	Store(scope string) (*dhcp.LeaseStore, error)
	Import(ctx context.Context, scope, format string, data []byte) (dhcp.ImportResult, error)
	Config(scope *v1alpha1.DHCPScope) (dhcp.Config, error)
}

// dhcpKubeNode replaces the node name in ETags and cursors of the kube
// store: they stay valid on every replica.
const dhcpKubeNode = "kubernetes"

// kubeStore reports the Kubernetes store, where DHCP setup is not per node
// even while dhcp.enabled is off.
func (s *Server) kubeStore() bool {
	cfg := s.config()
	return cfg != nil && cfg.Store.Driver == "kube"
}

func (s *Server) dhcpCursorNode() string {
	if s.opts.DHCPKube != nil {
		return dhcpKubeNode
	}
	return s.opts.NodeName
}

// dhcpScopeWritable reports whether the dashboard may change an object of
// owner: unmanaged, API-managed and bootstrap objects. Git-managed scopes
// carry another kudurru.io/managed-by value, e.g. gitops:argocd.
func dhcpScopeWritable(owner string) bool { return apiMayDelete(owner) }

func (s *Server) kubeDHCPScopes(ctx context.Context) ([]*v1alpha1.DHCPScope, error) {
	list, err := s.opts.Store.List(ctx, v1alpha1.KindDHCPScope, store.ListOptions{})
	if err != nil {
		return nil, storeError(err)
	}
	out := make([]*v1alpha1.DHCPScope, 0, len(list.Items))
	for _, item := range list.Items {
		if scope, ok := item.(*v1alpha1.DHCPScope); ok {
			out = append(out, scope)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) kubeDHCPScope(ctx context.Context, name string) (*v1alpha1.DHCPScope, error) {
	var scope v1alpha1.DHCPScope
	err := s.opts.Store.Get(ctx, store.Key{Kind: v1alpha1.KindDHCPScope, Name: name}, &scope)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNotFound("DHCP scope " + name)
	}
	if err != nil {
		return nil, storeError(err)
	}
	return &scope, nil
}

func kubeDHCPETag(scope *v1alpha1.DHCPScope) string {
	raw, _ := json.Marshal([]string{dhcpKubeNode, scope.Name, scope.ResourceVersion})
	return `"` + base64.RawURLEncoding.EncodeToString(raw) + `"`
}

func dhcpDuration(d metav1.Duration, fallback time.Duration) string {
	if d.Duration == 0 {
		return fallback.String()
	}
	return d.Duration.String()
}

func (s *Server) kubeDHCPStatus(ctx context.Context) (*dhcpStatusOutput, error) {
	st := s.opts.DHCPKube.Status()
	out := &DHCPStatus{Mode: dhcpKubeNode, Node: s.opts.NodeName, Enabled: true, Available: st.Listening,
		ServingNode: st.Leader, ServerAddress: st.Identifier.String(), RuntimeError: st.Error, Scopes: []DHCPScopeSummary{}}
	if len(st.Invalid) > 0 {
		out.InvalidScopes = st.Invalid
	}
	scopes, err := s.kubeDHCPScopes(ctx)
	if err != nil {
		return nil, err
	}
	for _, scope := range scopes {
		if !slices.Contains(st.Scopes, scope.Name) {
			continue
		}
		summary := DHCPScopeSummary{Name: scope.Name, Management: "api", Revision: strconv.FormatInt(scope.Generation, 10),
			Interface: st.Interface, ServerIP: st.Identifier.String(), Subnet: scope.Spec.Subnet, RelayOnly: true}
		if owner := store.ManagedBy(scope); !dhcpScopeWritable(owner) {
			summary.Management, summary.ManagedBy = "file", owner
		}
		out.Scopes = append(out.Scopes, summary)
	}
	return &dhcpStatusOutput{Body: out}, nil
}

func (s *Server) kubeDHCPConfig(scope *v1alpha1.DHCPScope) *dhcpConfigOutput {
	st := s.opts.DHCPKube.Status()
	spec := scope.Spec
	tag := kubeDHCPETag(scope)
	out := &DHCPConfig{Node: s.opts.NodeName, Scope: scope.Name, Management: "api", Revision: strconv.FormatInt(scope.Generation, 10), ETag: tag,
		Topology: DHCPTopology{ConflictDetection: spec.ConflictDetection, ProbeTimeout: dhcpOptionalDuration(spec.ProbeTimeout.Duration),
			Interface: st.Interface, ServerIP: st.Identifier.String(), Subnet: spec.Subnet, RelayOnly: true, TrustedRelays: []DHCPTrustedRelay{}},
		Options: DHCPPoolOptions{NTPServers: spec.NTPServers, DomainSearch: spec.DomainSearch, BootServer: spec.BootServer, BootServerName: spec.BootServerName, BootFile: spec.BootFile,
			RangeStart: spec.RangeStart, RangeEnd: spec.RangeEnd, Router: spec.Router, DNSServers: append([]string{}, spec.DNSServers...),
			DomainName: spec.DomainName, LeaseDuration: dhcpDuration(spec.LeaseDuration, 12*time.Hour), RegisterDNS: spec.RegisterDNS,
			DNSTTL: dhcpDuration(spec.DNSTTL, 30*time.Second)},
		Reservations: []DHCPReservation{}, EnvLocked: []string{}}
	if owner := store.ManagedBy(scope); !dhcpScopeWritable(owner) {
		out.Management, out.ManagedBy = "file", owner
	}
	for _, r := range spec.TrustedRelays {
		out.Topology.TrustedRelays = append(out.Topology.TrustedRelays, DHCPTrustedRelay{SourceIP: r.SourceIP, GatewayIP: r.GatewayIP,
			SourcePort: uint16(r.SourcePort), LinkSelection: r.LinkSelection, ServerIdentifier: r.ServerIdentifier}) // #nosec G115 -- the CRD bounds the port
	}
	for _, r := range spec.ClasslessRoutes {
		out.Options.ClasslessRoutes = append(out.Options.ClasslessRoutes, config.DHCPClasslessRoute{Destination: r.Destination, Router: r.Router})
	}
	for _, p := range spec.BootProfiles {
		out.Options.BootProfiles = append(out.Options.BootProfiles, config.DHCPBootProfile{Architecture: uint16(p.Architecture), BootServer: p.BootServer, BootServerName: p.BootServerName, BootFile: p.BootFile}) // #nosec G115 -- the CRD bounds the architecture
	}
	for _, r := range spec.Reservations {
		out.Reservations = append(out.Reservations, DHCPReservation{MAC: r.MAC, IP: r.IP, Hostname: r.Hostname})
	}
	return &dhcpConfigOutput{ETag: tag, Body: out}
}

// kubeApplyOptions copies the mutable options of the dashboard into spec.
func kubeApplyOptions(spec *v1alpha1.DHCPScopeSpec, o DHCPPoolOptions) error {
	lease, err := time.ParseDuration(o.LeaseDuration)
	if err != nil {
		return errInvalid("leaseDuration: %v", err)
	}
	ttl, err := time.ParseDuration(o.DNSTTL)
	if err != nil {
		return errInvalid("dnsTTL: %v", err)
	}
	spec.RangeStart, spec.RangeEnd, spec.Router = o.RangeStart, o.RangeEnd, o.Router
	spec.DNSServers = append([]string{}, o.DNSServers...)
	spec.DomainName, spec.RegisterDNS = o.DomainName, o.RegisterDNS
	spec.LeaseDuration, spec.DNSTTL = metav1.Duration{Duration: lease}, metav1.Duration{Duration: ttl}
	spec.NTPServers, spec.DomainSearch = o.NTPServers, o.DomainSearch
	spec.BootServer, spec.BootServerName, spec.BootFile = o.BootServer, o.BootServerName, o.BootFile
	spec.ClasslessRoutes, spec.BootProfiles = nil, nil
	for _, r := range o.ClasslessRoutes {
		spec.ClasslessRoutes = append(spec.ClasslessRoutes, v1alpha1.DHCPClasslessRoute{Destination: r.Destination, Router: r.Router})
	}
	for _, p := range o.BootProfiles {
		spec.BootProfiles = append(spec.BootProfiles, v1alpha1.DHCPBootProfile{Architecture: int32(p.Architecture), BootServer: p.BootServer, BootServerName: p.BootServerName, BootFile: p.BootFile})
	}
	return nil
}

// kubeCheckScope validates a candidate scope and, when it is running,
// that no live lease conflicts with it; the dashboard refuses such edits
// (a Git change applies anyway and reports the conflict in its status).
func (s *Server) kubeCheckScope(ctx context.Context, scope *v1alpha1.DHCPScope) error {
	cfg, err := s.opts.DHCPKube.Config(scope)
	if err != nil {
		return errInvalid("DHCP scope %s: %v", scope.Name, err)
	}
	if leases, err := s.opts.DHCPKube.Store(scope.Name); err == nil {
		existing := leases.Configuration().Config
		if dhcp.SameExternalTopology(existing, cfg) {
			if err := leases.CheckExternalConfig(cfg); err != nil {
				return dhcpError(err)
			}
		} else if live, err := leases.List(ctx); err != nil || len(live) > 0 {
			return errConflict("DHCP scope "+scope.Name+" has live leases; its subnet, relays and conflict detection cannot change",
				"wait for its leases to expire, or create a new scope")
		}
	}
	return nil
}

// kubeApplyScope validates and writes one DHCPScope as manager api, with
// next's resourceVersion (when set) as the precondition.
func (s *Server) kubeApplyScope(ctx context.Context, existing, next *v1alpha1.DHCPScope) (*v1alpha1.DHCPScope, error) {
	next.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindDHCPScope}
	var old store.Object
	opts := store.ApplyOptions{Manager: store.ManagerAPI}
	if existing != nil {
		owner := store.ManagedBy(existing)
		if !dhcpScopeWritable(owner) {
			return nil, errManagedBy(store.KeyOf(existing), owner)
		}
		old, opts.ExpectedResourceVersion, opts.Force = existing, existing.ResourceVersion, owner == store.ManagerBootstrap
	}
	if err := s.opts.Validator.Validate(ctx, next, old); err != nil {
		return nil, storeError(err)
	}
	if err := s.kubeCheckScope(ctx, next); err != nil {
		return nil, err
	}
	written, err := s.opts.Store.Apply(ctx, next, opts)
	if err != nil {
		return nil, storeError(err)
	}
	scope, ok := written.(*v1alpha1.DHCPScope)
	if !ok {
		return nil, errUnavailable("DHCP scope write returned %T", written)
	}
	return scope, nil
}

// kubeDHCPWrite edits one scope under its ETag.
func (s *Server) kubeDHCPWrite(ctx context.Context, name, tag, action string, edit func(*v1alpha1.DHCPScope) error) (*dhcpConfigOutput, error) {
	current, err := s.kubeDHCPScope(ctx, name)
	if err != nil {
		return nil, err
	}
	before, after := s.kubeDHCPConfig(current), (*dhcpConfigOutput)(nil)
	apply := func() error {
		if tag == "" {
			return errInvalid("If-Match: required")
		}
		if want := kubeDHCPETag(current); tag != want {
			return errPrecondition(tag, want)
		}
		next := current.DeepCopy()
		if err := edit(next); err != nil {
			return err
		}
		written, err := s.kubeApplyScope(ctx, current, next)
		if err != nil {
			return err
		}
		after = s.kubeDHCPConfig(written)
		return nil
	}
	err = apply()
	afterBody := before.Body
	if after != nil {
		afterBody = after.Body
	}
	s.recordAction(ctx, action, audit.Resource{Kind: "dhcp", Name: name}, err, before.Body, afterBody)
	if err != nil {
		return nil, kubeDHCPError(err)
	}
	return after, nil
}

// kubeDHCPError turns a store conflict into the 412 the dashboard reloads on.
func kubeDHCPError(err error) error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusConflict && apiErr.Body.Key == KeyConflict && strings.Contains(apiErr.Body.Message, "resourceVersion") {
			return newError(http.StatusPreconditionFailed, KeyPrecondition, "DHCP scope changed since it was read", "reload the scope and retry")
		}
		return apiErr
	}
	return storeError(err)
}

func kubeSetupETag(scopes []*v1alpha1.DHCPScope) (revision, tag string) {
	h := sha256.New()
	for _, scope := range scopes {
		h.Write([]byte(scope.Name + "=" + scope.ResourceVersion + "\n"))
	}
	revision = hex.EncodeToString(h.Sum(nil))[:16]
	raw, _ := json.Marshal([]string{dhcpKubeNode, "dhcp", "setup", revision})
	return revision, `"` + base64.RawURLEncoding.EncodeToString(raw) + `"`
}

func (s *Server) kubeDHCPSetupBody(scopes []*v1alpha1.DHCPScope) *dhcpSetupOutput {
	st := s.opts.DHCPKube.Status()
	revision, tag := kubeSetupETag(scopes)
	out := &DHCPSetup{Mode: dhcpKubeNode, Node: s.opts.NodeName, Enabled: true, ManagedBy: "api", Available: true,
		RuntimeError: st.Error, Revision: revision, ETag: tag, ServerAddress: st.Identifier.String(),
		Scopes: []DHCPSetupScope{}, ScopeRevisions: map[string]string{}, PendingScopes: []string{}}
	for _, scope := range scopes {
		cfg := s.kubeDHCPConfig(scope).Body
		out.Scopes = append(out.Scopes, DHCPSetupScope{NTPServers: cfg.Options.NTPServers, DomainSearch: cfg.Options.DomainSearch,
			ClasslessRoutes: cfg.Options.ClasslessRoutes, BootServer: cfg.Options.BootServer, BootServerName: cfg.Options.BootServerName,
			BootFile: cfg.Options.BootFile, BootProfiles: cfg.Options.BootProfiles, ConflictDetection: cfg.Topology.ConflictDetection,
			ProbeTimeout: cfg.Topology.ProbeTimeout, Name: scope.Name, Interface: cfg.Topology.Interface, ServerIP: cfg.Topology.ServerIP,
			Subnet: cfg.Topology.Subnet, RelayOnly: true, TrustedRelays: cfg.Topology.TrustedRelays, RangeStart: cfg.Options.RangeStart,
			RangeEnd: cfg.Options.RangeEnd, Router: cfg.Options.Router, DNSServers: cfg.Options.DNSServers, DomainName: cfg.Options.DomainName,
			LeaseDuration: cfg.Options.LeaseDuration, RegisterDNS: cfg.Options.RegisterDNS, DNSTTL: cfg.Options.DNSTTL, Reservations: cfg.Reservations})
		out.ScopeRevisions[scope.Name] = strconv.FormatInt(scope.Generation, 10)
		if cfg.ManagedBy != "" {
			if out.ScopeOwners == nil {
				out.ScopeOwners = map[string]string{}
			}
			out.ScopeOwners[scope.Name] = cfg.ManagedBy
		}
		// Applied by the serving replica once its status catches up.
		if scope.Status.ObservedGeneration < scope.Generation || !slices.Contains(st.Scopes, scope.Name) {
			out.PendingScopes = append(out.PendingScopes, scope.Name)
		}
	}
	return &dhcpSetupOutput{ETag: tag, Body: out}
}

func (s *Server) kubeDHCPSetup(ctx context.Context) (*dhcpSetupOutput, error) {
	scopes, err := s.kubeDHCPScopes(ctx)
	if err != nil {
		return nil, err
	}
	return s.kubeDHCPSetupBody(scopes), nil
}

// kubeSetupSpec converts a dashboard scope into a DHCPScope spec. Interface,
// server address and relay-only are node settings in Kubernetes and ignored.
func kubeSetupSpec(scope DHCPSetupScope) (v1alpha1.DHCPScopeSpec, error) {
	converted, err := scope.config()
	if err != nil {
		return v1alpha1.DHCPScopeSpec{}, err
	}
	spec := v1alpha1.DHCPScopeSpec{Subnet: scope.Subnet, ConflictDetection: scope.ConflictDetection,
		ProbeTimeout: metav1.Duration{Duration: converted.ProbeTimeout.Std()}, TrustedRelays: []v1alpha1.DHCPTrustedRelay{}}
	if err := kubeApplyOptions(&spec, DHCPPoolOptions{NTPServers: scope.NTPServers, DomainSearch: scope.DomainSearch,
		ClasslessRoutes: scope.ClasslessRoutes, BootServer: scope.BootServer, BootServerName: scope.BootServerName, BootFile: scope.BootFile,
		BootProfiles: scope.BootProfiles, RangeStart: scope.RangeStart, RangeEnd: scope.RangeEnd, Router: scope.Router,
		DNSServers: scope.DNSServers, DomainName: scope.DomainName, LeaseDuration: scope.LeaseDuration, RegisterDNS: scope.RegisterDNS,
		DNSTTL: scope.DNSTTL}); err != nil {
		return v1alpha1.DHCPScopeSpec{}, err
	}
	for _, r := range scope.TrustedRelays {
		spec.TrustedRelays = append(spec.TrustedRelays, v1alpha1.DHCPTrustedRelay{SourceIP: r.SourceIP, GatewayIP: r.GatewayIP,
			SourcePort: int32(r.SourcePort), LinkSelection: r.LinkSelection, ServerIdentifier: r.ServerIdentifier})
	}
	for _, r := range scope.Reservations {
		spec.Reservations = append(spec.Reservations, v1alpha1.DHCPReservation{MAC: r.MAC, IP: r.IP, Hostname: r.Hostname})
	}
	return spec, nil
}

// kubeDHCPPutSetup turns the dashboard's complete scope list into DHCPScope
// creates, updates and deletes. Every change is validated before the first
// write; the writes themselves are separate objects, each guarded by its
// resourceVersion.
func (s *Server) kubeDHCPPutSetup(ctx context.Context, in *dhcpSetupInput) (*dhcpSetupOutput, error) {
	current, err := s.kubeDHCPScopes(ctx)
	if err != nil {
		return nil, err
	}
	before := s.kubeDHCPSetupBody(current)
	after := before
	apply := func() error {
		if in.IfMatch != before.ETag {
			return errPrecondition(in.IfMatch, before.ETag)
		}
		if !in.Body.Enabled {
			return errConflict("DHCP is enabled by this release's settings (dhcp.enabled)", "set dhcp.enabled: false in the Helm values to turn DHCP off")
		}
		if in.Body.Failover != nil && in.Body.Failover.Enabled {
			return errInvalid("failover: in Kubernetes, one replica serves at a time through the kudurru-dhcp Lease")
		}
		byName := map[string]*v1alpha1.DHCPScope{}
		for _, scope := range current {
			byName[scope.Name] = scope
		}
		type change struct{ existing, next *v1alpha1.DHCPScope }
		var writes []change
		desired := map[string]bool{}
		for _, scope := range in.Body.Scopes {
			if desired[scope.Name] {
				return errInvalid("scope %q: duplicate name", scope.Name)
			}
			desired[scope.Name] = true
			spec, err := kubeSetupSpec(scope)
			if err != nil {
				return err
			}
			existing := byName[scope.Name]
			next := &v1alpha1.DHCPScope{ObjectMeta: metav1.ObjectMeta{Name: scope.Name}, Spec: spec}
			if existing != nil {
				if reflect.DeepEqual(existing.Spec, spec) {
					continue
				}
				next = existing.DeepCopy()
				next.Spec = spec
				if owner := store.ManagedBy(existing); !dhcpScopeWritable(owner) {
					return errManagedBy(store.KeyOf(existing), owner)
				}
			}
			if err := s.kubeCheckScope(ctx, next); err != nil {
				return err
			}
			writes = append(writes, change{existing, next})
		}
		// The serving replica would leave out a scope that overlaps
		// another; refuse the whole save instead.
		all := make([]dhcp.Config, 0, len(in.Body.Scopes))
		for _, scope := range in.Body.Scopes {
			spec, _ := kubeSetupSpec(scope)
			cfg, err := s.opts.DHCPKube.Config(&v1alpha1.DHCPScope{ObjectMeta: metav1.ObjectMeta{Name: scope.Name}, Spec: spec})
			if err != nil {
				return errInvalid("DHCP scope %s: %v", scope.Name, err)
			}
			all = append(all, cfg)
		}
		if err := dhcp.ValidateTopology(all); err != nil {
			return errInvalid("DHCP setup: %v", err)
		}
		var deletes []*v1alpha1.DHCPScope
		for _, scope := range current {
			if desired[scope.Name] {
				continue
			}
			if owner := store.ManagedBy(scope); !apiMayDelete(owner) {
				return errManagedBy(store.KeyOf(scope), owner)
			}
			deletes = append(deletes, scope)
		}
		for _, w := range writes {
			if _, err := s.kubeApplyScope(ctx, w.existing, w.next); err != nil {
				return err
			}
		}
		for _, scope := range deletes {
			err := s.opts.Store.Delete(ctx, store.KeyOf(scope), apiDeleteOptions(store.ManagedBy(scope)))
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return storeError(err)
			}
		}
		next, err := s.kubeDHCPScopes(ctx)
		if err != nil {
			return err
		}
		after = s.kubeDHCPSetupBody(next)
		return nil
	}
	err = apply()
	s.recordAction(ctx, "dhcp.setup", audit.Resource{Kind: "dhcp", Name: "setup"}, err, before.Body, after.Body)
	if err != nil {
		return nil, kubeDHCPError(err)
	}
	return after, nil
}
