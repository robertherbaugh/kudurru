package api

import (
	"context"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpkube"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// fakeDHCPCluster serves one running scope from a real external lease store.
type fakeDHCPCluster struct {
	stores map[string]*dhcp.LeaseStore
}

var kubeTestIdentifier = netip.MustParseAddr("198.51.100.67")

func (f *fakeDHCPCluster) Status() dhcpkube.Status {
	st := dhcpkube.Status{Interface: "eth0", Leader: "kudurru-0", Listening: true, Identifier: kubeTestIdentifier, Invalid: map[string]string{}}
	for name := range f.stores {
		st.Scopes = append(st.Scopes, name)
	}
	return st
}

func (f *fakeDHCPCluster) Store(scope string) (*dhcp.LeaseStore, error) {
	if s := f.stores[scope]; s != nil {
		return s, nil
	}
	return nil, dhcp.ErrUnavailable
}

func (f *fakeDHCPCluster) Import(ctx context.Context, scope, format string, data []byte) (dhcp.ImportResult, error) {
	s, err := f.Store(scope)
	if err != nil {
		return dhcp.ImportResult{}, err
	}
	return s.Import(ctx, format, data)
}

func (f *fakeDHCPCluster) Config(scope *v1alpha1.DHCPScope) (dhcp.Config, error) {
	return dhcpkube.ScopeConfig(scope, "eth0", netip.MustParseAddr("10.0.0.5"), kubeTestIdentifier)
}

func kubeTestScope(name string) DHCPSetupScope {
	return DHCPSetupScope{Name: name, Interface: "eth0", ServerIP: kubeTestIdentifier.String(), RelayOnly: true, Subnet: "192.0.2.0/24", RangeStart: "192.0.2.100", RangeEnd: "192.0.2.110",
		Router: "192.0.2.1", DNSServers: []string{"192.0.2.53"}, DomainName: "home.arpa", LeaseDuration: "12h", DNSTTL: "30s",
		ConflictDetection: "disabled", TrustedRelays: []DHCPTrustedRelay{{SourceIP: "192.0.2.1", GatewayIP: "192.0.2.1"}},
		Reservations: []DHCPReservation{}}
}

func kubeDHCPHarness(t *testing.T) (*harness, *fakeDHCPCluster) {
	t.Helper()
	cluster := &fakeDHCPCluster{stores: map[string]*dhcp.LeaseStore{}}
	h := newHarness(t, func(o *Options) { o.DHCPKube = cluster; o.Config.DHCP.Enabled = true })
	return h, cluster
}

// run starts a working copy for a scope that exists as an object.
func (f *fakeDHCPCluster) run(t *testing.T, h *harness, name string) *dhcp.LeaseStore {
	t.Helper()
	var scope v1alpha1.DHCPScope
	if err := h.st.Get(context.Background(), store.Key{Kind: v1alpha1.KindDHCPScope, Name: name}, &scope); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Config(&scope)
	if err != nil {
		t.Fatal(err)
	}
	s, err := dhcp.OpenExternalLeases(filepath.Join(t.TempDir(), name+".db"), cfg, func(context.Context, []dhcp.ExternalChange) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.stores[name] = s
	return s
}

func TestKubeDHCPSetupWritesScopes(t *testing.T) {
	h, cluster := kubeDHCPHarness(t)
	var setup DHCPSetup
	h.do(http.MethodGet, BasePath+dhcpBase+"/setup", "admin", nil).want(http.StatusOK).json(&setup)
	if setup.Mode != "kubernetes" || !setup.Enabled || len(setup.Scopes) != 0 || setup.ServerAddress != kubeTestIdentifier.String() {
		t.Fatalf("setup = %+v", setup)
	}
	// DHCP is a chart setting in Kubernetes.
	h.do(http.MethodPut, BasePath+dhcpBase+"/setup", "admin", DHCPSetupRequest{Enabled: false, Scopes: []DHCPSetupScope{}, ScopeRevisions: map[string]string{}},
		"If-Match", setup.ETag).want(http.StatusConflict)
	req := DHCPSetupRequest{Enabled: true, Scopes: []DHCPSetupScope{kubeTestScope("lan")}, ScopeRevisions: map[string]string{}}
	h.do(http.MethodPut, BasePath+dhcpBase+"/setup", "admin", req, "If-Match", setup.ETag).want(http.StatusOK).json(&setup)
	if len(setup.Scopes) != 1 || setup.Scopes[0].Name != "lan" || setup.Scopes[0].ServerIP != kubeTestIdentifier.String() || !setup.Scopes[0].RelayOnly {
		t.Fatalf("setup after save = %+v", setup.Scopes)
	}
	var scope v1alpha1.DHCPScope
	if err := h.st.Get(context.Background(), store.Key{Kind: v1alpha1.KindDHCPScope, Name: "lan"}, &scope); err != nil {
		t.Fatal(err)
	}
	if store.ManagedBy(&scope) != store.ManagerAPI || scope.Spec.LeaseDuration.Duration.String() != "12h0m0s" || len(scope.Spec.TrustedRelays) != 1 {
		t.Fatalf("object = %+v", scope)
	}
	// A stale setup ETag is refused.
	h.do(http.MethodPut, BasePath+dhcpBase+"/setup", "admin", req, "If-Match", `"stale"`).want(http.StatusPreconditionFailed)
	// An invalid scope is refused before anything is written.
	bad := kubeTestScope("broken")
	bad.RangeStart, bad.RangeEnd = "192.0.2.120", "192.0.2.110"
	bad.Subnet = "198.51.100.0/24"
	req.Scopes = append(req.Scopes, bad)
	h.do(http.MethodPut, BasePath+dhcpBase+"/setup", "admin", req, "If-Match", setup.ETag).want(http.StatusBadRequest)

	// Overlapping scopes are refused together.
	overlap := kubeTestScope("overlap")
	overlap.Subnet, overlap.RangeStart, overlap.RangeEnd = "192.0.2.128/25", "192.0.2.200", "192.0.2.210"
	req.Scopes = []DHCPSetupScope{kubeTestScope("lan"), overlap}
	h.do(http.MethodPut, BasePath+dhcpBase+"/setup", "admin", req, "If-Match", setup.ETag).want(http.StatusBadRequest)

	// Live options apply to the object under its ETag.
	cluster.run(t, h, "lan")
	cfg := dhcpAPIConfigScope(t, h, "lan")
	if cfg.Topology.ServerIP != kubeTestIdentifier.String() || cfg.Management != "api" {
		t.Fatalf("config = %+v", cfg)
	}
	opts := cfg.Options
	opts.DomainName = "lan.home.arpa"
	h.do(http.MethodPut, BasePath+dhcpBase+"/scopes/lan/config", "admin", opts, "If-Match", `"stale"`).want(http.StatusPreconditionFailed)
	var updated DHCPConfig
	h.do(http.MethodPut, BasePath+dhcpBase+"/scopes/lan/config", "admin", opts, "If-Match", cfg.ETag).want(http.StatusOK).json(&updated)
	if updated.Options.DomainName != "lan.home.arpa" || updated.ETag == cfg.ETag {
		t.Fatalf("updated = %+v", updated)
	}

	var status DHCPStatus
	h.do(http.MethodGet, BasePath+dhcpBase+"/status", "admin", nil).want(http.StatusOK).json(&status)
	if status.Mode != "kubernetes" || status.ServingNode != "kudurru-0" || len(status.Scopes) != 1 || status.Scopes[0].ServerIP != kubeTestIdentifier.String() {
		t.Fatalf("status = %+v", status)
	}
}

func dhcpAPIConfigScope(t *testing.T, h *harness, scope string) DHCPConfig {
	t.Helper()
	var cfg DHCPConfig
	r := h.do(http.MethodGet, BasePath+dhcpBase+"/scopes/"+scope+"/config", "admin", nil).want(http.StatusOK)
	r.json(&cfg)
	if cfg.ETag == "" || cfg.ETag != r.Header().Get("ETag") {
		t.Fatalf("ETag mismatch: %+v %s", cfg, r.Header().Get("ETag"))
	}
	return cfg
}

func TestKubeDHCPRespectsLeasesAndGitOwnership(t *testing.T) {
	h, cluster := kubeDHCPHarness(t)
	spec, err := kubeSetupSpec(kubeTestScope("lan"))
	if err != nil {
		t.Fatal(err)
	}
	h.seed(store.ManagerAPI, &v1alpha1.DHCPScope{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindDHCPScope},
		ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: spec})
	leases := cluster.run(t, h, "lan")
	lease := dhcpAPIBind(t, leases, 1)

	// Reserving the bound address for another client conflicts with the lease.
	cfg := dhcpAPIConfigScope(t, h, "lan")
	h.do(http.MethodPut, BasePath+dhcpBase+"/scopes/lan/reservations/02:00:00:00:00:99", "admin",
		map[string]string{"ip": lease.Address.String()}, "If-Match", cfg.ETag).want(http.StatusConflict)
	// Its own reservation is fine, at its address or at another one: the
	// client moves there at its next DISCOVER.
	var reserved DHCPConfig
	h.do(http.MethodPut, BasePath+dhcpBase+"/scopes/lan/reservations/02:00:00:00:00:01", "admin",
		map[string]string{"ip": lease.Address.String(), "hostname": "laptop"}, "If-Match", cfg.ETag).want(http.StatusOK).json(&reserved)
	h.do(http.MethodPut, BasePath+dhcpBase+"/scopes/lan/reservations/02:00:00:00:00:01", "admin",
		map[string]string{"ip": "192.0.2.50", "hostname": "laptop"}, "If-Match", reserved.ETag).want(http.StatusOK).json(&reserved)
	if len(reserved.Reservations) != 1 || reserved.Reservations[0].IP != "192.0.2.50" {
		t.Fatalf("reservations = %+v", reserved.Reservations)
	}
	var page DHCPLeases
	h.do(http.MethodGet, BasePath+dhcpBase+"/scopes/lan/leases", "admin", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.Items[0].Address != lease.Address.String() {
		t.Fatalf("leases = %+v", page)
	}

	// A scope managed in Git is read-only here and cannot be deleted.
	gitSpec := spec
	gitSpec.Subnet, gitSpec.RangeStart, gitSpec.RangeEnd, gitSpec.Router = "198.51.100.0/24", "198.51.100.100", "198.51.100.110", "198.51.100.1"
	gitSpec.TrustedRelays = []v1alpha1.DHCPTrustedRelay{{SourceIP: "198.51.100.1", GatewayIP: "198.51.100.1"}}
	gitSpec.Reservations = nil
	h.seed("gitops:argocd", &v1alpha1.DHCPScope{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindDHCPScope},
		ObjectMeta: metav1.ObjectMeta{Name: "iot"}, Spec: gitSpec})
	git := dhcpAPIConfigScope(t, h, "iot")
	if git.Management != "file" || git.ManagedBy != "gitops:argocd" {
		t.Fatalf("git scope = %+v", git)
	}
	r := h.do(http.MethodPut, BasePath+dhcpBase+"/scopes/iot/reservations/02:00:00:00:00:02", "admin",
		map[string]string{"ip": "198.51.100.120"}, "If-Match", git.ETag).want(http.StatusConflict)
	if r.errKey() != KeyManagedBy {
		t.Fatalf("error key = %s", r.errKey())
	}
	var setup DHCPSetup
	h.do(http.MethodGet, BasePath+dhcpBase+"/setup", "admin", nil).want(http.StatusOK).json(&setup)
	if setup.ScopeOwners["iot"] != "gitops:argocd" {
		t.Fatalf("owners = %+v", setup.ScopeOwners)
	}
	var keep []DHCPSetupScope
	for _, s := range setup.Scopes {
		if s.Name == "lan" {
			keep = append(keep, s)
		}
	}
	h.do(http.MethodPut, BasePath+dhcpBase+"/setup", "admin", DHCPSetupRequest{Enabled: true, Scopes: keep, ScopeRevisions: setup.ScopeRevisions},
		"If-Match", setup.ETag).want(http.StatusConflict)
}

func TestKubeDHCPOffInTheChart(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.Store.Driver = "kube" })
	var status DHCPStatus
	h.do(http.MethodGet, BasePath+dhcpBase+"/status", "admin", nil).want(http.StatusOK).json(&status)
	if status.Mode != "kubernetes" || status.Enabled {
		t.Fatalf("status = %+v", status)
	}
	var setup DHCPSetup
	h.do(http.MethodGet, BasePath+dhcpBase+"/setup", "admin", nil).want(http.StatusOK).json(&setup)
	if setup.Mode != "kubernetes" || setup.Available || setup.UnavailableReason == "" {
		t.Fatalf("setup = %+v", setup)
	}
}
