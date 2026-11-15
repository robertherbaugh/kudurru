package main

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// Client-group views on the Kubernetes driver (D403): the zone view built
// from informer data answers two ClientGroups differently for one name over
// UDP and TCP, a view-only name is NXDOMAIN for other clients, and a view
// change reaches the served zone.
func TestRunServeKubeClientGroupViews(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
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
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	cfg := offlineServeConfig(t, front.URL)
	cfg.Node.Name = "views-a"
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.DNSSEC.Enabled = false
	cfg.Listeners.DNS[0].Address = kubeRPZDualAddress(t)
	cfg.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	source, err := json.Marshal(map[string]any{"store": map[string]any{"driver": "kube"}, "dnssec": map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
	if err := os.WriteFile(cfg.Source, source, 0o600); err != nil {
		t.Fatal(err)
	}

	view := func(v string) *public.EndpointExt { return &public.EndpointExt{View: v} }
	records := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "split", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "split"}, Endpoints: []public.Endpoint{
		{DNSName: "www.split.test", RecordType: "A", Targets: []string{"203.0.113.10"}},
		{DNSName: "www.split.test", RecordType: "A", Targets: []string{"10.0.0.10"}, Kudurru: view("lan")},
		{DNSName: "www.split.test", RecordType: "A", Targets: []string{"10.9.0.10"}, Kudurru: view("kids")},
		{DNSName: "nas.split.test", RecordType: "A", Targets: []string{"10.0.0.20"}, Kudurru: view("lan")},
		{DNSName: "tunnel.split.test", RecordType: "A", Targets: []string{"198.51.100.200"}, Kudurru: view("public")},
	}}}
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "split"}, Spec: public.DNSZoneSpec{ZoneName: "split.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns1.split.test."}}},
		&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "lan", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.2"}}}},
		&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "kids", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.3"}}}},
		records,
	} {
		if err := admin.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	// The CRD schema refuses a view that cannot name a ClientGroup.
	bad := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "split"}, Endpoints: []public.Endpoint{
		{DNSName: "x.split.test", RecordType: "A", Targets: []string{"10.0.0.1"}, Kudurru: view("Not_A_Group")}}}}
	if err := admin.Create(t.Context(), bad); err == nil {
		t.Fatal("the apiserver accepted an invalid view name")
	}

	stop := startStaticMount(t, cfg)
	defer stop()
	answer := func(name, network, from string) (int, string) {
		m := kubeRPZExchange(cfg, name, network, net.ParseIP(from))
		if m == nil {
			return -1, ""
		}
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok {
				return m.Rcode, a.A.String()
			}
		}
		return m.Rcode, ""
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await("the lan view is served", func() bool {
		_, lan := answer("www.split.test.", "udp", "127.0.0.2")
		_, other := answer("www.split.test.", "udp", "127.0.0.1")
		return lan == "10.0.0.10" && other == "203.0.113.10"
	})
	for _, network := range []string{"udp", "tcp"} {
		for _, tc := range []struct {
			name, from string
			rcode      int
			want       string
		}{
			{"www.split.test.", "127.0.0.1", dns.RcodeSuccess, "203.0.113.10"},
			{"www.split.test.", "127.0.0.2", dns.RcodeSuccess, "10.0.0.10"},
			{"www.split.test.", "127.0.0.3", dns.RcodeSuccess, "10.9.0.10"},
			{"nas.split.test.", "127.0.0.1", dns.RcodeNameError, ""},
			{"nas.split.test.", "127.0.0.3", dns.RcodeNameError, ""},
			{"nas.split.test.", "127.0.0.2", dns.RcodeSuccess, "10.0.0.20"},
			{"tunnel.split.test.", "127.0.0.2", dns.RcodeNameError, ""},
		} {
			if rcode, got := answer(tc.name, network, tc.from); rcode != tc.rcode || got != tc.want {
				t.Fatalf("%s %s from %s: %d %q, want %d %q", network, tc.name, tc.from, rcode, got, tc.rcode, tc.want)
			}
		}
	}
	// A view change reaches the served zone.
	var current public.DNSRecordSet
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "split"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Endpoints[1].Targets = []string{"10.0.0.11"}
	if err := admin.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	await("the changed lan view is served", func() bool {
		_, lan := answer("www.split.test.", "tcp", "127.0.0.2")
		_, other := answer("www.split.test.", "tcp", "127.0.0.1")
		return lan == "10.0.0.11" && other == "203.0.113.10"
	})
}
