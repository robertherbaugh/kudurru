package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// This first-party structural fixture models the public MetalLB v0.15.3 L2
// status contract. No speaker or physical ARP/BGP announcer is simulated here.
func vipL2CRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "servicel2statuses.metallb.io"}, Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: "metallb.io", Scope: apiextensionsv1.NamespaceScoped, Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "servicel2statuses", Singular: "servicel2status", Kind: "ServiceL2Status", ListKind: "ServiceL2StatusList"}, Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Type: "object"}, "status": {Type: "object", XPreserveUnknownFields: &preserve}}}}, Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}}}}}}
}

func TestVIPObservationActualKubernetesAndMountedAPI(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for actual Kubernetes observation qualification")
	}
	control := &envtest.Environment{BinaryAssetsDirectory: assets, CRDs: []*apiextensionsv1.CustomResourceDefinition{vipL2CRD()}}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	installed, err := control.Start()
	if err != nil {
		t.Fatal("VIP envtest control plane failed to start")
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error("VIP envtest control plane failed to stop")
		}
	})
	admin, err := dynamic.NewForConfig(installed)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	gvr := func(group, version, resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	}
	services := gvr("", "v1", "services")
	nodes := gvr("", "v1", "nodes")
	pods := gvr("", "v1", "pods")
	leases := gvr("coordination.k8s.io", "v1", "leases")
	statuses := gvr("metallb.io", "v1beta1", "servicel2statuses")
	create := func(resource schema.GroupVersionResource, namespace string, obj map[string]any) *unstructured.Unstructured {
		t.Helper()
		value, err := admin.Resource(resource).Namespace(namespace).Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
		if err != nil {
			// Include API classifications, never request bodies or credentials.
			if status, ok := err.(apierrors.APIStatus); ok {
				info := status.Status()
				t.Logf("fixture API resource=%s reason=%s code=%d", resource.Resource, info.Reason, info.Code)
				if info.Details != nil {
					for _, cause := range info.Details.Causes {
						t.Logf("fixture API field=%s cause=%s", cause.Field, cause.Type)
					}
				}
			}
			t.Fatal("envtest fixture creation failed")
		}
		return value
	}
	for _, name := range []string{"apps", "metallb-system", "kube-node-lease"} {
		_, err := admin.Resource(gvr("", "v1", "namespaces")).Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	service := create(services, "apps", map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "dns", "annotations": map[string]any{"kube-vip.io/vipHost": "node-a"}}, "spec": map[string]any{"type": "LoadBalancer", "ports": []any{map[string]any{"port": int64(53), "protocol": "UDP"}}}})
	service.Object["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "192.0.2.53"}}}}
	service, err = admin.Resource(services).Namespace("apps").UpdateStatus(ctx, service, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node := create(nodes, "", map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "node-a"}})
	node.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	if _, err := admin.Resource(nodes).UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := create(pods, "metallb-system", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "speaker-a"}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "speaker", "image": "example.invalid/speaker-fixture"}}}})
	pod.Object["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	pod, err = admin.Resource(pods).Namespace("metallb-system").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Lease renewTime is metav1.MicroTime, whose JSON decoder requires six
	// fractional digits; RFC3339Nano can emit nine (or none).
	renew := time.Now().UTC().Add(-time.Second).Format(metav1.RFC3339Micro)
	for _, binding := range [][2]string{{"apps", "kubevip-dns"}, {"kube-node-lease", "node-a"}} {
		create(leases, binding[0], map[string]any{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": map[string]any{"name": binding[1]}, "spec": map[string]any{"holderIdentity": "node-a", "leaseDurationSeconds": int64(40), "renewTime": renew}})
	}
	status := create(statuses, "metallb-system", map[string]any{"apiVersion": "metallb.io/v1beta1", "kind": "ServiceL2Status", "metadata": map[string]any{"name": "l2-dns", "ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": "speaker-a", "uid": string(pod.GetUID())}}}})
	status.Object["status"] = map[string]any{"node": "node-a", "serviceName": "dns", "serviceNamespace": "apps", "interfaces": []any{map[string]any{"name": "eth0"}}}
	status, err = admin.Resource(statuses).Namespace("metallb-system").UpdateStatus(ctx, status, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	user, err := control.AddUser(envtest.User{Name: "vip-observer-readonly", Groups: []string{"system:authenticated"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules := []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"services", "nodes", "pods"}, "verbs": []any{"get"}}, map[string]any{"apiGroups": []any{"coordination.k8s.io"}, "resources": []any{"leases"}, "verbs": []any{"get"}}, map[string]any{"apiGroups": []any{"metallb.io"}, "resources": []any{"servicel2statuses"}, "verbs": []any{"get", "list"}}}
	create(gvr("rbac.authorization.k8s.io", "v1", "clusterroles"), "", map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": "vip-observer-readonly"}, "rules": rules})
	create(gvr("rbac.authorization.k8s.io", "v1", "clusterrolebindings"), "", map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": map[string]any{"name": "vip-observer-readonly"}, "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "vip-observer-readonly"}, "subjects": []any{map[string]any{"kind": "User", "apiGroup": "rbac.authorization.k8s.io", "name": "vip-observer-readonly"}}})
	restricted, err := dynamic.NewForConfig(user.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restricted.Resource(services).Namespace("apps").Update(ctx, service, metav1.UpdateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatal("observation credentials unexpectedly have Service writer authority")
	}
	for _, kind := range []string{"kube-vip", "metallb"} {
		t.Run(kind, func(t *testing.T) {
			cfg, err := config.Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.VIP.Observation.Provider = kind
			cfg.VIP.Observation.Services = []config.VIPServiceBinding{{Name: "dns", Address: "192.0.2.53", Namespace: "apps", Service: "dns"}}
			cfg.VIP.Observation.Interval = config.Duration(time.Second)
			read, run, err := serviceVIPObservation(cfg, user.Config())
			if err != nil || read == nil || run == nil {
				t.Fatal("production observation mount failed")
			}
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- run(runCtx) }()
			defer func() {
				stop()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Error("observer did not stop on original cancellation")
				}
			}()
			deadline := time.Now().Add(8 * time.Second)
			for !read().VIPs[0].Known && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			item := read().VIPs[0]
			if !item.Known || item.Owner != "node-a" || item.Evidence == nil {
				t.Fatal("actual apiserver announcer acquisition failed", item.Reason)
			}
			if kind == "metallb" && (item.Evidence.RenewedAt != nil || item.Evidence.ResourceVersion != status.GetResourceVersion()) {
				t.Fatal("MetalLB status reread invented provider heartbeat")
			}
			st := memory.New()
			defer func() { _ = st.Close() }()
			v, err := validator.New()
			if err != nil {
				t.Fatal(err)
			}
			router, err := api.New(api.Options{Store: st, Validator: v, Config: cfg, Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...), VIPObservation: read})
			if err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			request := func(identity string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/edge", nil)
				if identity != "" {
					r.Header.Set("Authorization", apitest.Bearer(identity))
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			var info api.EdgeInfo
			response := request("admin")
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &info) != nil || info.Observation == nil || info.Observation.VIPs[0].Owner != "node-a" {
				t.Fatal("mounted edge route lost observed evidence")
			}
			if response := request(""); response.Code != http.StatusUnauthorized {
				t.Fatal("anonymous observation read was admitted")
			}
			if response := request("zone-admin"); response.Code != http.StatusForbidden {
				t.Fatal("scoped principal gained cluster edge read permission")
			}
			// Expire actual source evidence, rather than merely advancing a
			// fake observer clock. A successful API reread cannot renew it.
			leaseNamespace, leaseName := "apps", "kubevip-dns"
			if kind == "metallb" {
				leaseNamespace, leaseName = "kube-node-lease", "node-a"
			}
			lease, err := admin.Resource(leases).Namespace(leaseNamespace).Get(ctx, leaseName, metav1.GetOptions{})
			if err != nil {
				t.Fatal("could not acquire lease fixture for expiry")
			}
			if err := unstructured.SetNestedField(lease.Object, time.Now().UTC().Add(-time.Hour).Format(metav1.RFC3339Micro), "spec", "renewTime"); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Resource(leases).Namespace(leaseNamespace).Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
				t.Fatal("could not expire actual source lease")
			}
			deadline = time.Now().Add(4 * time.Second)
			for read().VIPs[0].Known && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			response = request("admin")
			var expiredInfo api.EdgeInfo
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &expiredInfo) != nil || expiredInfo.Observation == nil || expiredInfo.Observation.VIPs[0].Known || expiredInfo.Observation.VIPs[0].Owner != "" || expiredInfo.Observation.VIPs[0].Reason != "stale" {
				t.Fatal("actual source expiry retained an owner in mounted API")
			}
			// A denied source cannot use admin fallback even though fixtures were
			// provisioned with a separate admin client.
			denied, err := control.AddUser(envtest.User{Name: "vip-observer-denied-" + kind, Groups: []string{"system:authenticated"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			deniedRead, deniedRun, err := serviceVIPObservation(cfg, denied.Config())
			if err != nil {
				t.Fatal(err)
			}
			deniedCtx, cancel := context.WithCancel(ctx)
			deniedDone := make(chan error, 1)
			go func() { deniedDone <- deniedRun(deniedCtx) }()
			deadline = time.Now().Add(3 * time.Second)
			for deniedRead().VIPs[0].Reason == "not_observed" && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			if err := <-deniedDone; !errors.Is(err, context.Canceled) {
				t.Fatal("denied observer did not stop on cancellation")
			}
			if deniedRead().VIPs[0].Known {
				t.Fatal("denied credentials borrowed admin observation authority")
			}
		})
	}
}
