package main

import (
	"encoding/json"
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

func TestRunServeDirectDNSEndpointDNSRestartAndDeletion(t *testing.T) {
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
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var identity corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &identity); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport, err = rest.TransportFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	sign := trustTestKey(t)
	t.Setenv(license.EnvKey, sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}}))
	t.Setenv(license.EnvKeyFile, "")
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Node.Name = "endpoint-mounted"
	cfg.Store.Kube.ExpectedClusterID = string(identity.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.ExternalDNS.CRD.Enabled = true
	cfg.ExternalDNS.CRD.Namespace = "default"
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	sourceConfig, err := json.Marshal(map[string]any{"store": map[string]any{"driver": "kube"}, "externalDNS": cfg.ExternalDNS, "blocking": map[string]any{"enabled": false}, "dnssec": map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
	if err := os.WriteFile(cfg.Source, sourceConfig, 0600); err != nil {
		t.Fatal(err)
	}
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "direct"}, Spec: public.DNSZoneSpec{ZoneName: "direct.example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.direct.example."}}}
	if err := admin.Create(t.Context(), zone); err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	resource := dyn.Resource(schema.GroupVersionResource{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}).Namespace("default")
	source, err := resource.Create(t.Context(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "externaldns.k8s.io/v1alpha1", "kind": "DNSEndpoint", "metadata": map[string]any{"name": "direct", "namespace": "default"},
		"spec": map[string]any{"endpoints": []any{map[string]any{"dnsName": "web.direct.example", "recordType": "A", "recordTTL": int64(30), "targets": []any{"192.0.2.30"}}}},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop := startStaticMount(t, cfg)
	query := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			q := new(dns.Msg)
			q.SetQuestion("web.direct.example.", dns.TypeA)
			answer, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
			if err == nil && answer.Authoritative {
				if want == "" && answer.Rcode == dns.RcodeNameError {
					return
				}
				if len(answer.Answer) == 1 {
					if a, ok := answer.Answer[0].(*dns.A); ok && a.A.String() == want {
						return
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("mounted DNS did not converge", want)
	}
	query("192.0.2.30")
	before, err := resource.Get(t.Context(), "direct", metav1.GetOptions{})
	if err != nil || before.GetResourceVersion() != source.GetResourceVersion() {
		t.Fatal("mounted controller changed source", err)
	}
	stop()
	_ = startStaticMount(t, cfg)
	query("192.0.2.30")
	if err := unstructured.SetNestedSlice(before.Object, []any{map[string]any{"dnsName": "web.direct.example", "recordType": "A", "recordTTL": int64(30), "targets": []any{"192.0.2.31"}}}, "spec", "endpoints"); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Update(t.Context(), before, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	query("192.0.2.31")
	if err := resource.Delete(t.Context(), "direct", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	query("")
	var records public.DNSRecordSetList
	if err := admin.List(t.Context(), &records, client.InNamespace("default"), client.MatchingLabels{"externaldns.kudurru.io/source": "crd"}); err != nil || len(records.Items) != 0 {
		t.Fatal("deleted source left generated children", err, len(records.Items))
	}
}
