package vip

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func evidenceObject(kind, name, namespace string, created time.Time) *unstructured.Unstructured {
	apiVersion := "v1"
	if kind == "Lease" {
		apiVersion = "coordination.k8s.io/v1"
	}
	if kind == "ServiceL2Status" {
		apiVersion = "metallb.io/v1beta1"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetUID(types.UID(kind + "-" + name))
	obj.SetResourceVersion("1")
	obj.SetCreationTimestamp(metav1.NewTime(created))
	return obj
}

func observationFixture(t *testing.T, kind string, now *time.Time) (*ServiceObserver, *dynamicfake.FakeDynamicClient, map[string]*unstructured.Unstructured) {
	t.Helper()
	objects := map[string]*unstructured.Unstructured{}
	service := evidenceObject("Service", "dns", "apps", now.Add(-4*time.Hour))
	service.Object["spec"] = map[string]any{"type": "LoadBalancer"}
	service.Object["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "192.0.2.53"}}}}
	service.SetAnnotations(map[string]string{"kube-vip.io/vipHost": "node-a", "kube-vip.io/loadbalancerIPs": "192.0.2.53", "metallb.io/loadBalancerIPs": "192.0.2.53"})
	objects["service"] = service
	node := evidenceObject("Node", "node-a", "", now.Add(-time.Hour))
	node.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	objects["node"] = node
	lease := evidenceObject("Lease", "kubevip-dns", "apps", now.Add(-3*time.Hour))
	lease.Object["spec"] = map[string]any{"holderIdentity": "node-a", "leaseDurationSeconds": int64(10), "renewTime": now.Add(-time.Second).Format(time.RFC3339Nano)}
	objects["lease"] = lease
	pod := evidenceObject("Pod", "speaker-a", "metallb-system", now.Add(-3*time.Hour))
	pod.Object["spec"] = map[string]any{"nodeName": "node-a"}
	pod.Object["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	objects["pod"] = pod
	status := evidenceObject("ServiceL2Status", "l2-dns", "metallb-system", now.Add(-2*time.Hour))
	status.Object["status"] = map[string]any{"node": "node-a", "serviceName": "dns", "serviceNamespace": "apps", "interfaces": []any{map[string]any{"name": "eth0"}}}
	status.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.GetName(), UID: pod.GetUID()}})
	objects["status"] = status
	nodeLease := evidenceObject("Lease", "node-a", "kube-node-lease", now.Add(-time.Hour))
	nodeLease.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.GetName(), UID: node.GetUID()}})
	nodeLease.Object["spec"] = map[string]any{"holderIdentity": "node-a", "leaseDurationSeconds": int64(40), "renewTime": now.Add(-time.Second).Format(time.RFC3339Nano)}
	objects["nodeLease"] = nodeLease
	items := []runtime.Object{}
	for _, obj := range objects {
		items = append(items, obj.DeepCopy())
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{l2GVR: "ServiceL2StatusList"}, items...)
	observer, err := NewServiceObserver(client, ServiceObserverConfig{Kind: kind, Bindings: []ServiceBinding{{Name: "dns", Address: "192.0.2.53", Namespace: "apps", Service: "dns"}}, AnnouncerNamespace: "metallb-system", Interval: time.Second, Timeout: time.Second, MaxAge: 5 * time.Second, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return observer, client, objects
}
func updateEvidence(t *testing.T, client *dynamicfake.FakeDynamicClient, obj *unstructured.Unstructured) {
	t.Helper()
	gvr := serviceGVR
	switch obj.GetKind() {
	case "Node":
		gvr = nodeGVR
	case "Pod":
		gvr = podGVR
	case "Lease":
		gvr = leaseGVR
	case "ServiceL2Status":
		gvr = l2GVR
	}
	if _, err := client.Resource(gvr).Namespace(obj.GetNamespace()).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceObserverCurrentEvidenceAndExpiry(t *testing.T) {
	for _, kind := range []string{"kube-vip", "metallb"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			observer, client, objects := observationFixture(t, kind, &now)
			if _, ok := observer.Owner("dns"); ok {
				t.Fatal("allocation created owner before observation")
			}
			observer.Refresh(context.Background())
			if owner, ok := observer.Owner("dns"); !ok || owner != "node-a" {
				t.Fatal("current announcer evidence not recognized")
			}
			item := observer.Snapshot().VIPs[0]
			if item.Evidence == nil || item.ServiceUID == "" || item.CheckedAt != now || !item.ExpiresAt.Equal(now.Add(5*time.Second)) {
				t.Fatal("original acquisition evidence missing")
			}
			if kind == "metallb" && (item.Evidence.RenewedAt != nil || item.Evidence.LivenessAt == nil || !item.Evidence.CreatedAt.Equal(objects["status"].GetCreationTimestamp().Time)) {
				t.Fatal("L2 reread invented provider heartbeat or lost old evidence age")
			}
			// Snapshot callers cannot alter current evidence, including nested times.
			item.Evidence.UID = "changed"
			if item.Evidence.RenewedAt != nil {
				*item.Evidence.RenewedAt = time.Time{}
			}
			if observer.Snapshot().VIPs[0].Evidence.UID == "changed" {
				t.Fatal("snapshot aliases live evidence")
			}
			now = now.Add(2 * time.Second)
			observer.Refresh(context.Background())
			if kind == "metallb" && observer.Snapshot().VIPs[0].Evidence.RenewedAt != nil {
				t.Fatal("polling manufactured a provider heartbeat")
			}
			now = now.Add(6 * time.Second)
			if _, ok := observer.Owner("dns"); ok {
				t.Fatal("stale local acquisition remains owner")
			}
			if observer.Snapshot().VIPs[0].Reason != "stale" {
				t.Fatal("missing stale diagnostic")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatal("observer issued a Kubernetes write")
				}
			}
		})
	}
}

func TestServiceObserverRefusesInsufficientEvidence(t *testing.T) {
	for _, kind := range []string{"kube-vip", "metallb"} {
		cases := map[string]func(map[string]*unstructured.Unstructured){
			"allocation only": func(o map[string]*unstructured.Unstructured) {
				o["service"].SetAnnotations(map[string]string{"kube-vip.io/loadbalancerIPs": "192.0.2.53", "metallb.io/loadBalancerIPs": "192.0.2.53"})
				o["status"].Object["status"] = map[string]any{}
			},
			"wrong allocation": func(o map[string]*unstructured.Unstructured) { o["service"].Object["status"] = map[string]any{} },
			"node unavailable": func(o map[string]*unstructured.Unstructured) { o["node"].Object["status"] = map[string]any{} },
			"announcer expired": func(o map[string]*unstructured.Unstructured) {
				for _, key := range []string{"lease", "nodeLease"} {
					_ = unstructured.SetNestedField(o[key].Object, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "spec", "renewTime")
				}
			},
			"wrong node": func(o map[string]*unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o["lease"].Object, "node-b", "spec", "holderIdentity")
				_ = unstructured.SetNestedField(o["status"].Object, "node-b", "status", "node")
			},
			"deleted Service": func(o map[string]*unstructured.Unstructured) {
				ts := metav1.Now()
				o["service"].SetDeletionTimestamp(&ts)
			},
			"Service reincarnation": func(o map[string]*unstructured.Unstructured) {
				o["service"].SetCreationTimestamp(metav1.Now())
				o["service"].SetUID("replacement")
			},
		}
		if kind == "metallb" {
			cases["replacement Node UID"] = func(o map[string]*unstructured.Unstructured) { o["node"].SetUID("replacement-node") }
			cases["unbound node heartbeat"] = func(o map[string]*unstructured.Unstructured) { o["nodeLease"].SetOwnerReferences(nil) }
			cases["ambiguous node heartbeat"] = func(o map[string]*unstructured.Unstructured) {
				o["nodeLease"].SetOwnerReferences(append(o["nodeLease"].GetOwnerReferences(), metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: "node-a", UID: "old-node"}))
			}
			cases["replacement speaker UID"] = func(o map[string]*unstructured.Unstructured) { o["pod"].SetUID("new-speaker") }
			cases["speaker unready"] = func(o map[string]*unstructured.Unstructured) {
				o["pod"].Object["status"] = map[string]any{"phase": "Running"}
			}
			cases["no interfaces"] = func(o map[string]*unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(o["status"].Object, []any{}, "status", "interfaces")
			}
			cases["no speaker provenance"] = func(o map[string]*unstructured.Unstructured) { o["status"].SetOwnerReferences(nil) }
		} else {
			cases["global election only"] = func(o map[string]*unstructured.Unstructured) { o["lease"].SetName("plndr-svcs-lock") }
		}
		for name, change := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				now := time.Now().UTC()
				observer, client, objects := observationFixture(t, kind, &now)
				observer.Refresh(context.Background())
				if _, ok := observer.Owner("dns"); !ok {
					t.Fatal("invalid test baseline")
				}
				change(objects)
				if name == "global election only" {
					if err := client.Resource(leaseGVR).Namespace("apps").Delete(context.Background(), "kubevip-dns", metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				for _, obj := range objects {
					if name == "global election only" && obj.GetKind() == "Lease" && obj.GetName() == "plndr-svcs-lock" {
						continue
					}
					updateEvidence(t, client, obj)
				}
				observer.Refresh(context.Background())
				if _, ok := observer.Owner("dns"); ok {
					t.Fatal("insufficient or stale announcer evidence admitted")
				}
			})
		}
	}
}

func TestServiceObserverAmbiguityFailureAndSourceFences(t *testing.T) {
	now := time.Now().UTC()
	observer, client, objects := observationFixture(t, "metallb", &now)
	duplicate := objects["status"].DeepCopy()
	duplicate.SetName("l2-other")
	duplicate.SetUID("other")
	if _, err := client.Resource(l2GVR).Namespace("metallb-system").Create(context.Background(), duplicate, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	observer.Refresh(context.Background())
	if item := observer.Snapshot().VIPs[0]; item.Known || item.Reason != "ambiguous" {
		t.Fatal("ambiguous announcers admitted")
	}
	if err := client.Resource(l2GVR).Namespace("metallb-system").Delete(context.Background(), duplicate.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	observer.Refresh(context.Background())
	if _, ok := observer.Owner("dns"); !ok {
		t.Fatal("current evidence not restored")
	}
	client.PrependReactor("get", "services", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(serviceGVR.GroupResource(), "dns", fmt.Errorf("denied"))
	})
	observer.Refresh(context.Background())
	if _, ok := observer.Owner("dns"); ok {
		t.Fatal("permission refusal kept previous owner")
	}
	for _, kind := range []string{"kube-vip", "metallb"} {
		t.Run(kind+"/original Service head", func(t *testing.T) {
			observer, client, objects := observationFixture(t, kind, &now)
			reads := 0
			client.PrependReactor("get", "services", func(ktesting.Action) (bool, runtime.Object, error) {
				reads++
				cloned := objects["service"].DeepCopy()
				if reads > 1 {
					cloned.SetUID("replacement")
				}
				return true, cloned, nil
			})
			observer.Refresh(context.Background())
			if item := observer.Snapshot().VIPs[0]; item.Known || item.Reason != "source_changed" {
				t.Fatal("replacement Service head admitted")
			}
		})
	}
}

func TestServiceObserverEvidenceCollectionBound(t *testing.T) {
	now := time.Now().UTC()
	observer, client, objects := observationFixture(t, "metallb", &now)
	for i := 0; i < 64; i++ {
		obj := objects["status"].DeepCopy()
		obj.SetName(fmt.Sprintf("l2-%d", i))
		obj.SetUID(types.UID(obj.GetName()))
		if _, err := client.Resource(l2GVR).Namespace("metallb-system").Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	observer.Refresh(context.Background())
	if item := observer.Snapshot().VIPs[0]; item.Known || item.Reason != "evidence_limit" {
		t.Fatal("unbounded status collection accepted")
	}
}

func TestServiceObserverClockRollbackAndOriginalLease(t *testing.T) {
	now := time.Now().UTC()
	observer, client, objects := observationFixture(t, "kube-vip", &now)
	_ = unstructured.SetNestedField(objects["lease"].Object, int64(3), "spec", "leaseDurationSeconds")
	updateEvidence(t, client, objects["lease"])
	observer.Refresh(context.Background())
	original := observer.Snapshot().VIPs[0]
	if !original.Known || !original.ExpiresAt.Equal(now.Add(2*time.Second)) {
		t.Fatal("provider original lease deadline was widened")
	}
	now = now.Add(-time.Second)
	if _, ok := observer.Owner("dns"); ok {
		t.Fatal("clock rollback admitted a future acquisition")
	}
	now = now.Add(4 * time.Second)
	observer.Refresh(context.Background())
	if _, ok := observer.Owner("dns"); ok {
		t.Fatal("reread renewed an expired provider lease")
	}
}

func TestServiceObserverNodeHeartbeatHeadChanges(t *testing.T) {
	for _, resource := range []string{"nodes", "leases"} {
		t.Run(resource, func(t *testing.T) {
			now := time.Now().UTC()
			observer, client, objects := observationFixture(t, "metallb", &now)
			reads := 0
			client.PrependReactor("get", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
				get := action.(ktesting.GetAction)
				if get.GetName() != "node-a" {
					return false, nil, nil
				}
				key := "node"
				if resource == "leases" {
					key = "nodeLease"
				}
				cloned := objects[key].DeepCopy()
				reads++
				if reads > 1 {
					cloned.SetResourceVersion("replacement-head")
				}
				return true, cloned, nil
			})
			observer.Refresh(t.Context())
			if item := observer.Snapshot().VIPs[0]; item.Known || item.Reason != "source_changed" {
				t.Fatal("changed node/heartbeat head established ownership")
			}
		})
	}
}
