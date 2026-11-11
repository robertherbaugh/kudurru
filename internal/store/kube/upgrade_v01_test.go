package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// v01CRDs are the CRDs of the 0.1.0 release, verbatim: the seven kudurru.io
// bases that the validator's fixtures hold, and the AuditLog and DHCPLease
// CRDs of testdata/v0.1.0 (README.md there).
func v01CRDs(t *testing.T) []*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	bases, err := loadCRDs(filepath.Join(repoRoot(), "internal", "store", "validator", "testdata", "v0.1.0", "crds"))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := loadCRDs(filepath.Join("testdata", "v0.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bases) != 7 || len(extra) != 2 {
		t.Fatalf("0.1.0 CRDs: %d bases and %d others, want 7 and 2", len(bases), len(extra))
	}
	return append(bases, extra...)
}

// currentCRDs are the CRDs of this checkout, as the CRD chart installs them.
func currentCRDs(t *testing.T) []*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var all []*apiextensionsv1.CustomResourceDefinition
	for _, dir := range []string{"bases", "audit", "dhcp"} {
		crds, err := loadCRDs(filepath.Join(repoRoot(), "deploy", "kustomize", "crd", dir))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, crds...)
	}
	return all
}

// v01Object reads a 0.1.0 sample as the unstructured object 0.1.0 sent.
func v01Object(t *testing.T, file string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(), "internal", "store", "validator", "testdata", "v0.1.0", "samples", file)) //nolint:gosec // test fixtures
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(b, &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

// v01Write stores u as the 0.1.0 kube driver did for an API write: a
// server-side apply with field manager "api" and the managed-by annotation
// (git show v0.1.0:internal/store/kube/crud.go), unless u already names
// another manager, as a GitOps object does.
func v01Write(ctx context.Context, t *testing.T, c client.Client, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	if ann[v1alpha1.ManagedByAnnotation] == "" {
		ann[v1alpha1.ManagedByAnnotation] = store.ManagerAPI
	}
	u.SetAnnotations(ann)
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(fieldManagerFor(ann[v1alpha1.ManagedByAnnotation]))); err != nil {
		t.Fatalf("0.1.0 write of %s %s: %v", u.GetKind(), u.GetName(), err)
	}
	return u
}

// v01Status writes the status 0.1.0 reported for u through the status
// subresource, as its controller did (field manager kudurru-controller).
func v01Status(ctx context.Context, t *testing.T, c client.Client, u *unstructured.Unstructured) {
	t.Helper()
	status := map[string]any{"conditions": []any{map[string]any{
		"type": v1alpha1.ConditionReady, "status": "True", "reason": "Serving", "message": "reported by 0.1.0",
		"observedGeneration": u.GetGeneration(), "lastTransitionTime": "2026-10-18T19:00:00Z",
	}}}
	if u.GetKind() == "UnblockRequest" {
		status = map[string]any{"state": "Pending", "note": "reported by 0.1.0"}
	}
	u.Object["status"] = status
	if err := c.Status().Update(ctx, u, client.FieldOwner(statusFieldManager)); err != nil {
		t.Fatalf("0.1.0 status of %s %s: %v", u.GetKind(), u.GetName(), err)
	}
}

// installCRDs replaces each CRD by name, as `helm upgrade` of the CRD chart
// does (the chart keeps them: helm.sh/resource-policy keep), or creates it.
func installCRDs(ctx context.Context, t *testing.T, c client.Client, crds []*apiextensionsv1.CustomResourceDefinition) {
	t.Helper()
	for _, want := range crds {
		var cur apiextensionsv1.CustomResourceDefinition
		err := c.Get(ctx, client.ObjectKey{Name: want.Name}, &cur)
		switch {
		case apierrors.IsNotFound(err):
			next := want.DeepCopy()
			if err := c.Create(ctx, next); err != nil {
				t.Fatalf("create CRD %s: %v", want.Name, err)
			}
		case err != nil:
			t.Fatal(err)
		default:
			cur.Spec = *want.Spec.DeepCopy()
			cur.Annotations = want.Annotations
			if err := c.Update(ctx, &cur); err != nil {
				t.Fatalf("update CRD %s: %v", want.Name, err)
			}
		}
	}
}

// crdUpgraded reports whether the CRD named like want serves want's schema:
// Established and NamesAccepted for the generation that carries it, with
// v1alpha1 as the only stored version.
func crdUpgraded(ctx context.Context, c client.Client, want *apiextensionsv1.CustomResourceDefinition) (bool, string) {
	var cur apiextensionsv1.CustomResourceDefinition
	if err := c.Get(ctx, client.ObjectKey{Name: want.Name}, &cur); err != nil {
		return false, err.Error()
	}
	if !reflect.DeepEqual(cur.Spec.Versions[0].Schema, want.Spec.Versions[0].Schema) {
		return false, "schema not replaced"
	}
	conds := map[apiextensionsv1.CustomResourceDefinitionConditionType]apiextensionsv1.ConditionStatus{}
	for _, cond := range cur.Status.Conditions {
		conds[cond.Type] = cond.Status
	}
	if conds[apiextensionsv1.Established] != apiextensionsv1.ConditionTrue || conds[apiextensionsv1.NamesAccepted] != apiextensionsv1.ConditionTrue {
		return false, "not established"
	}
	if !slices.Equal(cur.Status.StoredVersions, []string{"v1alpha1"}) {
		return false, "stored versions " + strings.Join(cur.Status.StoredVersions, ",")
	}
	return true, ""
}

// TestEnvtestCRDUpgradeFromV01 is the Kubernetes half of the upgrade from
// 0.1.0 (D373): a cluster that ran 0.1.0, with its CRDs, its objects and
// their status, gets the CRDs of this release, and this release's driver
// then reads, updates and cold-starts from what 0.1.0 stored.
//
//  1. The 0.1.0 CRDs are installed and every 0.1.0 sample is written as the
//     0.1.0 driver wrote it, with status, together with objects in the
//     `default` namespace (where 0.1.0 installations kept their objects
//     before the release-namespace defaults) and two objects 0.1.0 accepted
//     that this release's schema refuses (TestV01ObjectsThisReleaseRefuses).
//     A field new in this release is pruned by the 0.1.0 schema.
//  2. The CRDs are replaced in place by this checkout's and the five new
//     ones are created. Every CRD is Established with v1alpha1 the only
//     stored version: there is no conversion and no storage migration.
//  3. This release's driver lists and reads every object with its UID,
//     spec and status. Status updates work on every object, including the
//     refused ones; an unchanged-spec write of a refused object works
//     (CRD validation ratcheting) while a changed one is refused, naming
//     the rule, until the object is corrected; a write with a new field
//     keeps it.
//  4. zonecache writes local.db from these objects, and a replica that
//     starts with the apiserver gone serves them from that file.
func TestEnvtestCRDUpgradeFromV01(t *testing.T) {
	requireEnv(t)
	assets, _ := findEnvtestAssets()
	env, cfg, admin, err := startControlPlaneWith(assets, v01CRDs(t))
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = env.Stop()
		}
	})
	ctx := t.Context()
	var ks corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ks); err != nil {
		t.Fatal(err)
	}

	// 1. What 0.1.0 stored.
	samples := []string{
		"kudurru.io_v1alpha1_dnszone.yaml", "kudurru.io_v1alpha1_dnsrecordset.yaml", "kudurru.io_v1alpha1_blocklist.yaml",
		"kudurru.io_v1alpha1_clientgroup.yaml", "kudurru.io_v1alpha1_policy.yaml", "kudurru.io_v1alpha1_unblockrequest.yaml",
		"kudurru.io_v1alpha1_dhcpscope.yaml",
	}
	var written []*unstructured.Unstructured
	for _, f := range samples {
		written = append(written, v01Object(t, f))
	}
	obj := func(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": kind, "spec": spec}}
		u.SetNamespace(namespace)
		u.SetName(name)
		return u
	}
	// The 0.1.0 driver sent its Go type, whose spec.policy is a struct
	// without omitempty effect: an empty policy, which the schema defaults.
	legacyZone := obj("DNSZone", "", "legacy-secondary", map[string]any{"zoneName": "legacy.example.", "type": "secondary", "policy": map[string]any{}})
	legacyPolicy := obj("Policy", "default", "legacy-rewrite", map[string]any{"rewrites": []any{map[string]any{"name": "a.example."}}})
	written = append(written,
		obj("DNSRecordSet", "default", "router", map[string]any{
			"zoneRef":   map[string]any{"name": "home-example-com"},
			"endpoints": []any{map[string]any{"dnsName": "router.home.example.com", "recordType": "A", "targets": []any{"10.0.0.1"}}},
		}),
		obj("ClientGroup", "default", "lan", map[string]any{"clients": []any{map[string]any{"cidr": "10.0.0.0/24"}}}),
		legacyZone, legacyPolicy,
	)
	// A placeholder stub zone written with kubectl (no managed-by annotation,
	// no policy), as GitOps installations of 0.1.0 wrote objects.
	kubectlZone := obj("DNSZone", "", "legacy-stub", map[string]any{"zoneName": "stub.legacy.example.", "type": "stub"})
	if err := admin.Create(ctx, kubectlZone, client.FieldOwner("kubectl-client-side-apply")); err != nil {
		t.Fatal(err)
	}
	for _, u := range written {
		if u.GetNamespace() == "" && u.GetKind() != "DNSZone" && u.GetKind() != "DHCPScope" {
			t.Fatalf("%s %s has no namespace", u.GetKind(), u.GetName())
		}
		v01Write(ctx, t, admin, u)
	}
	stored := map[store.Key]v01Identity{}
	for _, u := range written {
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(u.GroupVersionKind())
		if err := admin.Get(ctx, client.ObjectKeyFromObject(u), cur); err != nil {
			t.Fatal(err)
		}
		v01Status(ctx, t, admin, cur)
		if err := admin.Get(ctx, client.ObjectKeyFromObject(u), cur); err != nil {
			t.Fatal(err)
		}
		key := store.Key{Kind: store.Kind(cur.GetKind()), Namespace: cur.GetNamespace(), Name: cur.GetName()}
		typed, err := typedFrom(s0(t), cur)
		if err != nil {
			t.Fatalf("this release cannot decode %s as 0.1.0 stored it: %v", key, err)
		}
		stored[key] = v01Identity{uid: string(cur.GetUID()), rv: cur.GetResourceVersion(), gen: cur.GetGeneration(), spec: typed["spec"], status: typed["status"]}
	}
	// The 0.1.0 schema has no ClientGroup.spec.rateLimitExempt: server-side
	// apply refuses it and a create prunes it.
	pruned := obj("ClientGroup", "default", "pruned", map[string]any{"clients": []any{map[string]any{"cidr": "10.0.1.0/24"}}, "rateLimitExempt": true})
	if err := admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(pruned.DeepCopy()), client.FieldOwner("api")); err == nil || !strings.Contains(err.Error(), "rateLimitExempt") {
		t.Fatalf("the 0.1.0 CRDs took a field of this release: %v", err)
	}
	if err := admin.Create(ctx, pruned); err != nil {
		t.Fatal(err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(pruned), pruned); err != nil {
		t.Fatal(err)
	}
	if _, kept, _ := unstructured.NestedBool(pruned.Object, "spec", "rateLimitExempt"); kept {
		t.Fatal("the 0.1.0 CRDs kept a field of this release: they are not the 0.1.0 schema")
	}
	if err := admin.Delete(ctx, pruned); err != nil {
		t.Fatal(err)
	}

	// 2. The CRDs of this release replace them.
	crdScheme := testScheme()
	if err := apiextensionsv1.AddToScheme(crdScheme); err != nil {
		t.Fatal(err)
	}
	crdClient, err := client.New(cfg, client.Options{Scheme: crdScheme})
	if err != nil {
		t.Fatal(err)
	}
	current := currentCRDs(t)
	installCRDs(ctx, t, crdClient, current)
	for _, want := range current {
		eventually(t, 30*time.Second, "CRD "+want.Name+" upgraded", func() bool {
			ok, _ := crdUpgraded(ctx, crdClient, want)
			return ok
		})
	}
	if len(current) != 14 {
		t.Fatalf("%d CRDs in this checkout; the upgrade covers 9 of 0.1.0 and 5 new ones", len(current))
	}

	// 3. This release's driver on the upgraded cluster.
	s := newStoreOn(t, cfg, Options{Namespace: "kudurru-system", ExpectedClusterID: string(ks.UID)})
	if !s.WaitForSync(ctx) {
		t.Fatal("the driver did not sync on the upgraded CRDs")
	}
	listed := map[store.Key]bool{}
	for kind := range kindsOf(stored) {
		page, err := s.List(ctx, kind, store.ListOptions{})
		if err != nil {
			t.Fatalf("list %s: %v", kind, err)
		}
		for _, o := range page.Items {
			listed[store.KeyOf(o)] = true
		}
	}
	for key, was := range stored {
		if !listed[key] {
			t.Errorf("%s is not listed after the upgrade", key)
		}
		o, err := getObject(ctx, s, key)
		if err != nil {
			t.Fatalf("get %s after the upgrade: %v", key, err)
		}
		if string(o.GetUID()) != was.uid || o.GetResourceVersion() != was.rv || o.GetGeneration() != was.gen {
			t.Errorf("%s changed identity: uid %s rv %s gen %d, was %s %s %d", key, o.GetUID(), o.GetResourceVersion(), o.GetGeneration(), was.uid, was.rv, was.gen)
		}
		u, err := toUnstructured(o)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(u["spec"], was.spec) {
			t.Errorf("%s spec after the upgrade\n got %v\nwant %v", key, u["spec"], was.spec)
		}
		if !reflect.DeepEqual(u["status"], was.status) {
			t.Errorf("%s status after the upgrade\n got %v\nwant %v", key, u["status"], was.status)
		}
	}

	// Status updates, as this release's controllers make them, on every object.
	for key := range stored {
		o, err := getObject(ctx, s, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := setReadyStatus(o, "reported by 0.2"); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateStatus(ctx, o); err != nil {
			t.Errorf("status update of %s after the upgrade: %v", key, err)
			continue
		}
		after, err := getObject(ctx, s, key)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := toUnstructured(after); !strings.Contains(stringOf(got["status"]), "reported by 0.2") {
			t.Errorf("status of %s not updated: %v", key, got["status"])
		}
		if after.GetGeneration() != stored[key].gen {
			t.Errorf("status update of %s changed its generation", key)
		}
	}

	// The API's write of an object it read back unchanged (here with a new
	// label), for every object 0.1.0 wrote through the API.
	for key := range stored {
		o, err := getObject(ctx, s, key)
		if err != nil {
			t.Fatal(err)
		}
		if store.ManagedBy(o) != store.ManagerAPI {
			continue // the GitOps DHCPScope: its owner writes it
		}
		labels := o.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["upgraded"] = "true"
		o.SetLabels(labels)
		o.SetResourceVersion("")
		if _, err := s.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Errorf("unchanged-spec write of %s after the upgrade: %v", key, err)
		}
	}

	// The kubectl-written placeholder: this release's driver reads it and
	// updates its status, and kubectl can still relabel it (ratcheting).
	// The driver's own write of it would add the empty spec.policy of the Go
	// type, which the schema defaults: a spec change that the zone rule
	// refuses until the zone is corrected, as any changed spec is.
	stubKey := store.Key{Kind: "DNSZone", Name: "legacy-stub"}
	stub, err := getObject(ctx, s, stubKey)
	if err != nil {
		t.Fatal(err)
	}
	if stub.(*v1alpha1.DNSZone).Spec.Type != v1alpha1.ZoneTypeStub || stub.(*v1alpha1.DNSZone).Spec.Stub != nil {
		t.Fatalf("the kubectl-written stub zone reads as %+v", stub.(*v1alpha1.DNSZone).Spec)
	}
	stub.(*v1alpha1.DNSZone).Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonInvalid, Message: "stub zones require nameserver configuration", LastTransitionTime: metav1.Now(), ObservedGeneration: stub.GetGeneration()}}
	if err := s.UpdateStatus(ctx, stub); err != nil {
		t.Fatalf("status of the kubectl-written stub zone: %v", err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(kubectlZone), kubectlZone); err != nil {
		t.Fatal(err)
	}
	kubectlZone.SetLabels(map[string]string{"upgraded": "true"})
	if err := admin.Update(ctx, kubectlZone); err != nil {
		t.Fatalf("kubectl relabel of the 0.1.0 stub zone after the upgrade: %v", err)
	}

	// A changed spec of an object this release refuses is refused, naming
	// the rule, until the object is corrected.
	zoneKey := store.Key{Kind: "DNSZone", Name: "legacy-secondary"}
	var z v1alpha1.DNSZone
	if err := s.Get(ctx, zoneKey, &z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Nameservers = []string{"ns1.legacy.example."}
	z.ResourceVersion = ""
	_, err = s.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerAPI})
	if invalid, ok := validator.AsError(err); !ok || !strings.Contains(invalid.Error(), "secondary zones require secondary configuration") {
		t.Fatalf("a changed spec of the 0.1.0 secondary zone without configuration: %v", err)
	}
	z.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}}
	if _, err := s.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("correcting the 0.1.0 secondary zone: %v", err)
	}
	policyKey := store.Key{Kind: "Policy", Namespace: "default", Name: "legacy-rewrite"}
	var p v1alpha1.Policy
	if err := s.Get(ctx, policyKey, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Rewrites = append(p.Spec.Rewrites, v1alpha1.Rewrite{Name: "b.example.", Targets: []string{"192.0.2.2"}})
	p.ResourceVersion = ""
	_, err = s.Apply(ctx, &p, store.ApplyOptions{Manager: store.ManagerAPI})
	if invalid, ok := validator.AsError(err); !ok || !strings.Contains(invalid.Error(), "provide either a CNAME or targets") {
		t.Fatalf("a changed 0.1.0 rewrite list with the rewrite this release refuses: %v", err)
	}
	p.Spec.Rewrites = []v1alpha1.Rewrite{{Name: "a.example.", Targets: []string{"192.0.2.1"}}}
	if _, err := s.Apply(ctx, &p, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("correcting the 0.1.0 rewrite: %v", err)
	}

	// Fields new in this release are kept now.
	groupKey := store.Key{Kind: "ClientGroup", Namespace: "dns", Name: "kids"}
	var g v1alpha1.ClientGroup
	if err := s.Get(ctx, groupKey, &g); err != nil {
		t.Fatal(err)
	}
	g.Spec.RateLimitExempt = true
	g.ResourceVersion = ""
	if _, err := s.Apply(ctx, &g, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("write with a field of this release: %v", err)
	}
	var kids v1alpha1.Policy
	if err := s.Get(ctx, store.Key{Kind: "Policy", Namespace: "dns", Name: "kids"}, &kids); err != nil {
		t.Fatal(err)
	}
	filter := true
	kids.Spec.Response = &v1alpha1.PolicyResponse{FilterAAAA: &filter}
	kids.ResourceVersion = ""
	if _, err := s.Apply(ctx, &kids, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("write with a field of this release: %v", err)
	}
	g = v1alpha1.ClientGroup{}
	kids = v1alpha1.Policy{}
	if err := s.Get(ctx, groupKey, &g); err != nil || !g.Spec.RateLimitExempt {
		t.Fatalf("ClientGroup.spec.rateLimitExempt after the write: %v %v", g.Spec.RateLimitExempt, err)
	}
	if err := s.Get(ctx, store.Key{Kind: "Policy", Namespace: "dns", Name: "kids"}, &kids); err != nil || kids.Spec.Response == nil || kids.Spec.Response.FilterAAAA == nil || !*kids.Spec.Response.FilterAAAA {
		t.Fatalf("Policy.spec.response after the write: %+v %v", kids.Spec.Response, err)
	}

	// 4. local.db from these objects, and a cold start from it.
	dbPath := filepath.Join(t.TempDir(), localdb.FileName)
	db, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	zones := zonecache.NewBuilder("zones", func(set zonecache.ObjectSet) (any, error) {
		out := map[string][]string{}
		for _, z := range set.Zones() {
			names := []string{}
			for _, rs := range set.RecordSetsForZone(z.Name) {
				names = append(names, rs.Namespace+"/"+rs.Name)
			}
			slices.Sort(names)
			out[z.Name] = names
		}
		return out, nil
	})
	wantZones := map[string][]string{"home-example-com": {"apps/nas", "default/router"}, "legacy-secondary": {}, "legacy-stub": {}}
	live, err := zonecache.New(s, zonecache.Options{LocalDB: db, ExpectedClusterID: string(ks.UID), PersistInterval: 100 * time.Millisecond, Debounce: 10 * time.Millisecond, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Register(zones); err != nil {
		t.Fatal(err)
	}
	stopLive := runCache(t, live)
	// DefaultKinds: three zones, two record sets, one list, two groups
	// ("pruned" was deleted) and two policies.
	const watched = 3 + 2 + 1 + 2 + 2
	eventually(t, 30*time.Second, "local.db with every object 0.1.0 stored", func() bool {
		snap, err := db.Load(ctx)
		return err == nil && len(snap.Objects) == watched && snap.ClusterID == string(ks.UID)
	})
	stopLive()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := env.Stop(); err != nil {
		t.Fatal(err)
	}
	stopped = true

	coldStore := newStoreOn(t, cfg, Options{Namespace: "kudurru-system", ExpectedClusterID: string(ks.UID), StartupTimeout: time.Second})
	cold, err := zonecache.New(coldStore, zonecache.Options{LocalDB: db, ExpectedClusterID: string(ks.UID), StartupTimeout: time.Second, Debounce: 10 * time.Millisecond, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := cold.Register(zones); err != nil {
		t.Fatal(err)
	}
	runCache(t, cold)
	// The cache turns FromSnapshot before it rebuilds the views (the
	// rebuild runs after Debounce), so wait on the published snapshot,
	// not on the cache state.
	var snap *zonecache.Snapshot
	eventually(t, 15*time.Second, fmt.Sprintf("zones view %v served from local.db", wantZones), func() bool {
		s := cold.Snapshot()
		if s == nil || s.State != zonecache.FromSnapshot {
			return false
		}
		if got, ok := zonecache.View[map[string][]string](s, "zones"); !ok || !reflect.DeepEqual(got, wantZones) {
			return false
		}
		snap = s
		return true
	})
	if snap.Objects.Len() != watched || !snap.Authoritative() {
		t.Fatalf("cold start: %d objects, authoritative %t", snap.Objects.Len(), snap.Authoritative())
	}
	if ok, msgs := cold.Ready(); !ok {
		t.Fatalf("not ready from a fresh local.db: %v", msgs)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// v01Identity is what 0.1.0 stored for an object, decoded by this release.
type v01Identity struct {
	uid, rv      string
	gen          int64
	spec, status any
}

func kindsOf(m map[store.Key]v01Identity) map[store.Kind]bool {
	out := map[store.Kind]bool{}
	for k := range m {
		out[k.Kind] = true
	}
	return out
}

// s0 is the kind registry of this release.
func s0(t *testing.T) *registry {
	t.Helper()
	reg, err := newRegistry(testScheme())
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// typedFrom decodes an apiserver object into this release's Go type and
// encodes it again, the shape the driver returns.
func typedFrom(reg *registry, u *unstructured.Unstructured) (map[string]any, error) {
	ki, err := reg.get(store.Kind(u.GetKind()))
	if err != nil {
		return nil, err
	}
	o := ki.newObject()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, o); err != nil {
		return nil, err
	}
	return toUnstructured(o)
}

func toUnstructured(o store.Object) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(o)
}

// getObject reads key through the driver into its Go type.
func getObject(ctx context.Context, s *Store, key store.Key) (store.Object, error) {
	ki, err := s.kinds.get(key.Kind)
	if err != nil {
		return nil, err
	}
	o := ki.newObject()
	if err := s.Get(ctx, key, o); err != nil {
		return nil, err
	}
	return o, nil
}

// setReadyStatus replaces the status message 0.1.0 reported.
func setReadyStatus(o store.Object, message string) error {
	u, err := toUnstructured(o)
	if err != nil {
		return err
	}
	if store.KindOf(o) == "UnblockRequest" {
		if err := unstructured.SetNestedField(u, message, "status", "note"); err != nil {
			return err
		}
	} else {
		conds, _, _ := unstructured.NestedSlice(u, "status", "conditions")
		if len(conds) != 1 {
			return errors.New("no condition of 0.1.0 on " + string(store.KindOf(o)))
		}
		conds[0].(map[string]any)["message"] = message
		if err := unstructured.SetNestedSlice(u, conds, "status", "conditions"); err != nil {
			return err
		}
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u, o)
}

func stringOf(v any) string {
	b, _ := yaml.Marshal(v)
	return string(b)
}

// runCache starts c and returns a function that stops it and waits.
func runCache(t *testing.T, c *zonecache.Cache) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("zonecache: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("zonecache did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}
