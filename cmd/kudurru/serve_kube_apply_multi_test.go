package main

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// The Kubernetes driver has no serving-generation gate, so a multi-document
// request was never cut there. This holds it to the same contract as the
// complete SQLite runtime (D378): one POST /apply with three new record sets
// creates all three, the node serves them, the same request again reports three
// unchanged documents, and one batch delete removes all three.
func TestRunServeKubeMultiDocumentApplyAndBatchDelete(t *testing.T) {
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
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	startStaticMount(t, cfg)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	c := &http.Client{Transport: transport, Jar: jar, Timeout: 20 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	tsigMountAwaitRead(t, c, base+"/auth/status", nil)
	var session api.SessionInfo
	controlMountedRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "offline-test-password-1"}, http.StatusOK, &session)

	zone := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-multi"},
		Spec: public.DNSZoneSpec{ZoneName: "kube-multi.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.kube-multi.test."}}}
	var applied api.List[api.ApplyResult]
	controlMountedRequest(t, c, base, http.MethodPost, "/apply", session.CSRF, []any{zone}, http.StatusOK, &applied)
	if len(applied.Items) != 1 || applied.Items[0].Result != api.ResultCreated {
		t.Fatalf("zone apply on Kubernetes failed: %+v", applied.Items)
	}
	names := []string{"k0", "k1", "k2"}
	var documents []any
	for i, name := range names {
		documents = append(documents, &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "kube-multi"}, Endpoints: []public.Endpoint{{DNSName: name + ".kube-multi.test.", RecordType: "A", Targets: []string{fmt.Sprintf("198.51.100.%d", 20+i)}, RecordTTL: 60}}}})
	}
	applied = api.List[api.ApplyResult]{}
	controlMountedRequest(t, c, base, http.MethodPost, "/apply", session.CSRF, documents, http.StatusOK, &applied)
	if len(applied.Items) != 3 {
		t.Fatalf("apply on Kubernetes did not answer one result for each document: %+v", applied.Items)
	}
	for _, item := range applied.Items {
		if item.Result != api.ResultCreated || item.Error != "" {
			t.Fatalf("one apply of three new record sets on Kubernetes did not create all of them: %+v", applied.Items)
		}
	}
	for i, name := range names {
		awaitMountedDNS(t, cfg, name+".kube-multi.test.", fmt.Sprintf("198.51.100.%d", 20+i))
	}
	applied = api.List[api.ApplyResult]{}
	controlMountedRequest(t, c, base, http.MethodPost, "/apply", session.CSRF, documents, http.StatusOK, &applied)
	for _, item := range applied.Items {
		if len(applied.Items) != 3 || item.Result != api.ResultUnchanged {
			t.Fatalf("second identical apply on Kubernetes did not report three unchanged documents: %+v", applied.Items)
		}
	}
	var deleted api.Processed
	controlMountedRequest(t, c, base, http.MethodPost, "/dnsrecordsets:batchDelete", session.CSRF, map[string]any{"names": names, "namespace": "default"}, http.StatusOK, &deleted)
	if len(deleted.Processed.Success) != 3 || len(deleted.Processed.Errors) != 0 {
		t.Fatalf("one batch delete on Kubernetes did not delete all three record sets: %+v", deleted.Processed)
	}
	for _, name := range names {
		awaitMountedDNSAbsent(t, cfg, name+".kube-multi.test.")
		var stored public.DNSRecordSet
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, &stored); err == nil {
			t.Fatalf("record set %s survived the batch delete on Kubernetes", name)
		}
	}
}
