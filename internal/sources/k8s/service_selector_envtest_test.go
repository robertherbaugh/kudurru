package k8s

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func TestExternalSourceServiceSelectorEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		if os.Getenv("KUDURRU_REQUIRE_ENVTEST") == "1" {
			t.Fatal("KUBEBUILDER_ASSETS is required")
		}
		t.Skip("set KUBEBUILDER_ASSETS to run the Service selector envtest")
	}
	useExistingCluster := false
	control := envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: &useExistingCluster}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	config, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	services := client.CoreV1().Services("default")
	create := func(name string, serviceType corev1.ServiceType) *corev1.Service {
		t.Helper()
		service, err := services.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
				annotationPrefix + "hostname": name + ".ext.example.",
				annotationPrefix + "target":   "192.0.2.42",
			}},
			Spec: corev1.ServiceSpec{Type: serviceType, Ports: []corev1.ServicePort{{Port: 80}}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	lb := create("external-lb", corev1.ServiceTypeLoadBalancer)
	internal := create("external-internal", corev1.ServiceTypeClusterIP)
	filtered, err := services.List(ctx, metav1.ListOptions{FieldSelector: "spec.type=LoadBalancer"})
	selector := "spec.type=LoadBalancer"
	if apierrors.IsBadRequest(err) {
		t.Logf("server requires unfiltered Service lists: %v", err)
		selector = ""
	} else if err != nil || len(filtered.Items) != 1 || filtered.Items[0].UID != lb.UID {
		t.Fatalf("unexpected Service selector result: %v, %v", filtered, err)
	} else {
		t.Log("server supports the LoadBalancer field selector")
	}

	st := memory.New()
	zone := externalZone()
	zone.Spec.External = &v1alpha1.ExternalSpec{LoadBalancerServices: true}
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := New(config, Options{Store: st, Validator: v, Admit: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	kind := kindOf("service")
	items, _, err := controller.list(ctx, kind)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := range items {
		seen[string(items[i].GetUID())] = true
	}
	if !seen[string(lb.UID)] || seen[string(internal.UID)] != (selector == "") {
		t.Fatalf("unexpected Service list with selector %q: %v", selector, seen)
	}
	watches, err := controller.reconcile(ctx)
	if err != nil || len(watches) != 1 || watches[0].selector != selector {
		t.Fatalf("watch did not follow the complete Service list: %v, %v", watches, err)
	}
	child := func(service *corev1.Service) error {
		return st.Get(ctx, store.Key{Kind: "DNSRecordSet", Namespace: service.Namespace, Name: RecordSetName(kind.label, string(service.UID), zone.Name)}, &v1alpha1.DNSRecordSet{})
	}
	if err := child(lb); err != nil {
		t.Fatal("LoadBalancer record set missing:", err)
	}
	if err := child(internal); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("ClusterIP must not publish even on the unfiltered fallback:", err)
	}
	stream, err := controller.client.Resource(kind.gvr).Watch(ctx, metav1.ListOptions{ResourceVersion: watches[0].rv, FieldSelector: watches[0].selector})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	lb.Spec.Type, lb.Spec.ExternalTrafficPolicy, lb.Spec.Ports[0].NodePort = corev1.ServiceTypeClusterIP, "", 0
	if _, err := services.Update(ctx, lb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wantEvent := watch.Deleted
	if selector == "" {
		wantEvent = watch.Modified
	}
	for matched := false; !matched; {
		select {
		case <-ctx.Done():
			t.Fatal("watch missed Service type transition:", ctx.Err())
		case event, ok := <-stream.ResultChan():
			if !ok {
				t.Fatal("watch ended before Service type transition")
			}
			object, ok := event.Object.(*unstructured.Unstructured)
			matched = ok && object.GetUID() == lb.UID && event.Type == wantEvent
		}
	}
	if _, err := controller.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := child(lb); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("same UID no longer a LoadBalancer must withdraw its record set:", err)
	}
}
