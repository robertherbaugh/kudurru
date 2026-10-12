package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// kubeStore starts kube-apiserver and etcd with the kudurru CRDs and
// returns the kube driver over them, or skips when KUBEBUILDER_ASSETS is
// unset (or KUDURRU_SKIP_ENVTEST is set).
func kubeStore(t *testing.T) *kube.Store {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	switch {
	case os.Getenv("KUDURRU_SKIP_ENVTEST") != "":
		t.Skip("KUDURRU_SKIP_ENVTEST is set")
	case assets == "":
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make envtest` and export it to run the kube driver tests")
	}
	for _, b := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(assets, b)); err != nil { //nolint:gosec // KUBEBUILDER_ASSETS is the test operator's own setting
			t.Skipf("KUBEBUILDER_ASSETS=%s has no %s", assets, b)
		}
	}
	if os.Getenv("KUDURRU_TEST_LOGS") == "" {
		ctrllog.SetLogger(logr.Discard())
		klog.SetLogger(logr.Discard())
	}
	files, err := validator.EmbeddedCRDs()
	if err != nil {
		t.Fatal(err)
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for name, b := range files {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(b, crd); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		crds = append(crds, crd)
	}
	env := &envtest.Environment{CRDs: crds, BinaryAssetsDirectory: assets}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"apps", "home", "kudurru-system"} {
		if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := kube.New(cfg, kube.Options{Namespace: "kudurru-system", Identity: "api-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), StartupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestDeleteZoneKube runs the cascade on the kube driver (no multi-object
// transaction): record sets in every namespace first, the zone last,
// nothing left referencing the zone; a managed record set refuses the
// whole delete before anything is removed.
func TestDeleteZoneKube(t *testing.T) {
	st := kubeStore(t)
	h := newHarness(t, func(o *Options) { o.Store = st })
	ctx := context.Background()
	apply := func(manager string, objs ...store.Object) {
		t.Helper()
		for _, o := range objs {
			if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: manager}); err != nil {
				t.Fatalf("seed %s: %v", store.KeyOf(o), err)
			}
		}
	}
	apply(store.ManagerAPI, zoneObj("example", "example.com."), zoneObj("other", "other.org."))
	apply(store.ManagerAPI,
		recordSetObj("default", "api-example", "example", ep("www.example.com", "A", "192.0.2.1")),
		recordSetObj("apps", "web", "example", ep("app.example.com", "A", "192.0.2.2"), ep("api.example.com", "A", "192.0.2.3")),
		recordSetObj("home", "nas", "other", ep("nas.other.org", "A", "192.0.2.4")))
	apply(store.ManagerIntegration("externaldns"), recordSetObj("home", "ext", "other", ep("svc.other.org", "A", "192.0.2.5")))

	var dry ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "admin", nil).want(http.StatusOK).json(&dry)
	if dry.RecordSetCount != 2 || dry.EndpointCount != 3 {
		t.Fatalf("dry run %+v", dry)
	}
	var res ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example", "zoneadmin", nil).want(http.StatusOK).json(&res)
	if res.RecordSetCount != 2 || res.EndpointCount != 3 {
		t.Fatalf("delete %+v", res)
	}
	if h.exists(zoneKey("example")) {
		t.Fatal("zone survived")
	}
	sets, err := h.srv.zoneRecordSets(ctx, "example")
	if err != nil || len(sets) != 0 {
		t.Fatalf("orphaned record sets %v (%v)", sets, err)
	}

	// A record set of an integration: refused, nothing deleted.
	r := h.do(http.MethodDelete, "/api/v1/zones/other", "admin", nil).want(http.StatusConflict)
	if e := errBody(r); e.Key != KeyManagedBy || len(e.Objects) != 1 || e.Objects[0].Name != "ext" {
		t.Fatalf("managed: %s", r.Body.String())
	}
	for _, k := range []store.Key{zoneKey("other"), rsKey("home", "nas"), rsKey("home", "ext")} {
		if !h.exists(k) {
			t.Fatalf("%s deleted by a refused request", k)
		}
	}
	// The generic route does not delete a referenced zone alone.
	h.do(http.MethodDelete, "/api/v1/objects/dnszones/other", "admin", nil).want(http.StatusConflict)
	if !h.exists(zoneKey("other")) {
		t.Fatal("zone deleted alone")
	}
}
