package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	dhcpv1 "github.com/robertherbaugh/kudurru/api/dhcp/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// kubeInventoryAnswer: blocked is the configured REFUSED answer; allowed is
// any other answer (the name is special-use, so no upstream is needed).
func kubeInventoryAnswer(a *dns.Msg, blocked bool) bool {
	return a != nil && (a.Rcode == dns.RcodeRefused) == blocked
}

// D397 on Kubernetes: a DHCPLease object (the durable lease on this driver)
// reaches the replica's in-memory lease snapshot, and its MAC makes a
// ClientGroup mac matcher select the group for UDP queries from the leased
// address (carried by the trusted PROXYv2 edge header). GET /inventory lists the lease; deleting it ends the match.
func TestRunServeKubeLeaseMACSelectsGroupAndInventory(t *testing.T) {
	useLoopbackDHCP(t)
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases", "../../deploy/kustomize/crd/dhcp"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	actual, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dhcpv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(actual, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(actual.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(actual); err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)

	for _, obj := range inventoryMACGroups("default", false) {
		if err := admin.Create(t.Context(), obj.(client.Object)); err != nil {
			t.Fatal("seed inventory objects", err)
		}
	}
	scope := &public.DHCPScope{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: public.DHCPScopeSpec{Subnet: "192.0.2.0/24", RangeStart: "192.0.2.100", RangeEnd: "192.0.2.110",
		Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"}, TrustedRelays: []public.DHCPTrustedRelay{{SourceIP: "198.51.100.1", GatewayIP: "192.0.2.1"}}}}
	if err := admin.Create(t.Context(), scope); err != nil {
		t.Fatal(err)
	}

	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	// Cluster settings come from the settings ConfigMap and the environment
	// on Kubernetes, not from this in-memory configuration. A REFUSED block
	// answer tells a blocked query apart from any upstream outcome.
	t.Setenv("KUDURRU_BLOCKING__DEFAULTRESPONSE", "refused")
	t.Setenv("KUDURRU_CACHE__MAXENTRIES", "0")
	inventoryEdge(t, cfg)
	cfg.DHCP = config.DHCP{Enabled: true, Management: "file", ServerIP: "192.0.2.67"}
	previous := dhcpKubeLocalAddress
	dhcpKubeLocalAddress = func(string) (netip.Addr, error) { return netip.MustParseAddr("198.51.100.2"), nil }
	t.Cleanup(func() { dhcpKubeLocalAddress = previous })
	startStaticMount(t, cfg)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	c := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	tsigMountAwaitRead(t, c, base+"/auth/status", nil)
	inventoryLogin(t, c, base, "offline-test-password-1")

	awaitInventoryAnswer(t, cfg, "192.0.2.100", "leased.test.", false, kubeInventoryAnswer)
	lease := &dhcpv1.DHCPLease{ObjectMeta: metav1.ObjectMeta{Name: "lan.192.0.2.100", Namespace: "default", Labels: map[string]string{dhcpv1.LabelScope: "lan"}},
		Spec: dhcpv1.DHCPLeaseSpec{Scope: "lan", Address: "192.0.2.100", Identity: dhcpv1.DHCPLeaseIdentity{Kind: "mac", Value: inventoryLeasedMAC}, MAC: inventoryLeasedMAC,
			Hostname: "laptop", ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour).UTC().Truncate(time.Second)), State: "bound"}}
	if err := admin.Create(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	awaitInventoryAnswer(t, cfg, "192.0.2.100", "leased.test.", true, kubeInventoryAnswer)
	awaitInventoryAnswer(t, cfg, "192.0.2.101", "leased.test.", false, kubeInventoryAnswer)

	var body api.InventoryBody
	inventoryAwait(t, c, base+"/inventory", 20*time.Second, "Kubernetes lease listed", &body, func() bool {
		d, ok := inventoryByKey(body)["dhcp/lan 192.0.2.100"]
		return ok && len(body.Items) == 1 && !d.Stale && d.Kind == api.InventoryKindDHCP && d.Hostname == "laptop" && len(d.MACs) == 1 && d.MACs[0] == inventoryLeasedMAC && inventoryAddressState(d, "192.0.2.100") == "assigned"
	})
	if body.Node != cfg.Node.Name {
		t.Fatalf("inventory node %q", body.Node)
	}

	if err := admin.Delete(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	awaitInventoryAnswer(t, cfg, "192.0.2.100", "leased.test.", false, kubeInventoryAnswer)
	inventoryAwait(t, c, base+"/inventory", 20*time.Second, "deleted lease unlisted", &body, func() bool { return len(body.Items) == 0 })
}
