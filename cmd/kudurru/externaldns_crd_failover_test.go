package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// partitionGate forwards one replica's API traffic and can cut it off,
// including already established watch connections, without stopping the
// replica process. It models a network partition from the API server.
type partitionGate struct {
	server  *httptest.Server
	blocked atomic.Bool
	mu      sync.Mutex
	conns   map[net.Conn]bool
}

func newPartitionGate(t *testing.T, restConfig *rest.Config) *partitionGate {
	t.Helper()
	target, err := url.Parse(restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(restConfig); err != nil {
		t.Fatal(err)
	}
	g := &partitionGate{conns: map[net.Conn]bool{}}
	g.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.blocked.Load() {
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	g.server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch state {
		case http.StateNew:
			g.conns[conn] = true
		case http.StateClosed, http.StateHijacked:
			delete(g.conns, conn)
		}
	}
	g.server.Start()
	t.Cleanup(g.server.Close)
	return g
}

func (g *partitionGate) partition(blocked bool) {
	g.blocked.Store(blocked)
	if !blocked {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for conn := range g.conns {
		_ = conn.Close()
	}
}

// Two mounted replicas reconcile one DNSEndpoint source through the elected
// controller Lease. Partitioning the leader from the API server hands the term
// to the other replica, which alone reconciles later source changes; after the
// partition heals the old leader neither regains the Lease nor duplicates
// children, and both replicas converge. Deletion still removes every child.
func TestRunServeDirectDNSEndpointLeaseLossAndPartition(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases", "../../ee/integration/externaldns/testdata"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, public.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var identity corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &identity); err != nil {
		t.Fatal(err)
	}
	sign := trustTestKey(t)
	t.Setenv(license.EnvKey, sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}}))
	t.Setenv(license.EnvKeyFile, "")
	replica := func(name string, gate *partitionGate) *config.Config {
		cfg := offlineServeConfig(t, gate.server.URL)
		cfg.Node.Name = name
		cfg.Store.Kube.ExpectedClusterID = string(identity.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.ExternalDNS.CRD.Enabled = true
		cfg.ExternalDNS.CRD.Namespace = "default"
		cfg.Blocking.Enabled = false
		cfg.DNSSEC.Enabled = false
		raw, err := json.Marshal(map[string]any{"store": map[string]any{"driver": "kube"}, "externalDNS": cfg.ExternalDNS, "blocking": map[string]any{"enabled": false}, "dnssec": map[string]any{"enabled": false}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
		if err := os.WriteFile(cfg.Source, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	gateA, gateB := newPartitionGate(t, restConfig), newPartitionGate(t, restConfig)
	// offlineServeConfig sets the process-global KUBECONFIG, which runServe
	// reads at startup: build and start each replica before the next config.
	cfgA := replica("endpoint-a", gateA)
	if err := admin.Create(t.Context(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "direct"}, Spec: public.DNSZoneSpec{ZoneName: "direct.example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.direct.example."}}}); err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	resource := dyn.Resource(schema.GroupVersionResource{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}).Namespace("default")
	if _, err := resource.Create(t.Context(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "externaldns.k8s.io/v1alpha1", "kind": "DNSEndpoint", "metadata": map[string]any{"name": "direct", "namespace": "default"},
		"spec": map[string]any{"endpoints": []any{map[string]any{"dnsName": "web.direct.example", "recordType": "A", "recordTTL": int64(30), "targets": []any{"192.0.2.30"}}}},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	answers := func(cfg *config.Config, want string) bool {
		q := new(dns.Msg)
		q.SetQuestion("web.direct.example.", dns.TypeA)
		answer, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
		if err != nil || !answer.Authoritative {
			return false
		}
		if want == "" {
			return answer.Rcode == dns.RcodeNameError
		}
		if len(answer.Answer) != 1 {
			return false
		}
		a, ok := answer.Answer[0].(*dns.A)
		return ok && a.A.String() == want
	}
	await := func(d time.Duration, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	holder := func() string {
		var lease coordinationv1.Lease
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "kudurru-controller"}, &lease); err != nil || lease.Spec.HolderIdentity == nil {
			return ""
		}
		return *lease.Spec.HolderIdentity
	}
	children := func() []public.DNSRecordSet {
		var records public.DNSRecordSetList
		if err := admin.List(t.Context(), &records, client.InNamespace("default"), client.MatchingLabels{"externaldns.kudurru.io/source": "crd"}); err != nil {
			t.Fatal(err)
		}
		return records.Items
	}
	_ = startStaticMount(t, cfgA)
	await(30*time.Second, "replica A leads", func() bool { return holder() == "endpoint-a" })
	await(30*time.Second, "A serves the generated record", func() bool { return answers(cfgA, "192.0.2.30") })
	cfgB := replica("endpoint-b", gateB)
	_ = startStaticMount(t, cfgB)
	await(30*time.Second, "B serves the generated record", func() bool { return answers(cfgB, "192.0.2.30") })
	if got := children(); len(got) != 1 || holder() != "endpoint-a" {
		t.Fatal("a second replica changed leadership or duplicated children", len(got), holder())
	}

	// Partition the leader from the API server. Its renewals fail, the Lease
	// expires and B acquires the term without any test intervention.
	gateA.partition(true)
	leaseState := func() string {
		var lease coordinationv1.Lease
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "kudurru-controller"}, &lease); err != nil {
			return err.Error()
		}
		raw, _ := json.Marshal(lease.Spec)
		return string(raw)
	}
	deadline := time.Now().Add(60 * time.Second)
	for holder() != "endpoint-b" {
		if time.Now().After(deadline) {
			t.Fatal("B never acquired the Lease after the leader's partition:", leaseState(), time.Now().UTC())
		}
		time.Sleep(50 * time.Millisecond)
	}
	source, err := resource.Get(t.Context(), "direct", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(source.Object, []any{map[string]any{"dnsName": "web.direct.example", "recordType": "A", "recordTTL": int64(30), "targets": []any{"192.0.2.31"}}}, "spec", "endpoints"); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Update(t.Context(), source, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	await(30*time.Second, "B reconciles the changed source", func() bool { return answers(cfgB, "192.0.2.31") })
	// The partitioned replica keeps answering from its last valid data.
	if !answers(cfgA, "192.0.2.30") {
		t.Fatal("partitioned replica stopped serving its last valid data")
	}

	// Heal: A observes B's successor term, never reclaims it, and converges.
	gateA.partition(false)
	await(30*time.Second, "A converges after the partition heals", func() bool { return answers(cfgA, "192.0.2.31") })
	if holder() != "endpoint-b" {
		t.Fatal("the healed former leader reclaimed the Lease", holder())
	}
	if got := children(); len(got) != 1 || len(got[0].Spec.Endpoints) != 1 || got[0].Spec.Endpoints[0].Targets[0] != "192.0.2.31" {
		t.Fatal("partition and heal duplicated or reverted generated children", len(got))
	}
	if err := resource.Delete(t.Context(), "direct", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	await(30*time.Second, "deletion withdraws the record on both replicas", func() bool { return answers(cfgA, "") && answers(cfgB, "") })
	if got := children(); len(got) != 0 {
		t.Fatal("deleted source left generated children", len(got))
	}
}
