package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func TestFieldManagerFor(t *testing.T) {
	long := "file:" + strings.Repeat("objects.d/", 30) + "zones.yaml"
	long2 := "file:" + strings.Repeat("objects.d/", 30) + "other.yaml"
	exact := strings.Repeat("a", 128)
	tests := []struct {
		name    string
		manager string
		same    bool // returned unchanged
	}{
		{"api", store.ManagerAPI, true},
		{"file", store.ManagerFile("objects.d/zones.yaml"), true},
		{"integration", store.ManagerIntegration("unifi"), true},
		{"exactly 128", exact, true},
		{"too long", long, false},
		{"non-printable", "file:zones\x00.yaml", false},
		{"unicode printable", "file:zönes.yaml", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fieldManagerFor(tt.manager)
			if tt.same {
				if got != tt.manager {
					t.Fatalf("fieldManagerFor(%q) = %q, want unchanged", tt.manager, got)
				}
				return
			}
			if got == tt.manager {
				t.Fatalf("fieldManagerFor(%q) returned the invalid value unchanged", tt.manager)
			}
			if len(got) > 128 {
				t.Fatalf("len = %d, want <= 128", len(got))
			}
			if !strings.Contains(got, "~") {
				t.Fatalf("missing hash suffix: %q", got)
			}
			if fieldManagerFor(tt.manager) != got {
				t.Fatal("not deterministic")
			}
		})
	}
	if fieldManagerFor(long) == fieldManagerFor(long2) {
		t.Fatal("distinct long managers must map to distinct field managers")
	}
}

func TestConflictingManagers(t *testing.T) {
	gr := schema.GroupResource{Group: "kudurru.io", Resource: "dnszones"}
	conflict := func(msgs ...string) error {
		causes := make([]metav1.StatusCause, 0, len(msgs))
		for _, m := range msgs {
			causes = append(causes, metav1.StatusCause{Type: metav1.CauseTypeFieldManagerConflict, Message: m, Field: ".spec.defaultTTL"})
		}
		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Code: 409, Reason: metav1.StatusReasonConflict,
			Details: &metav1.StatusDetails{Group: gr.Group, Kind: gr.Resource, Name: "home", Causes: causes},
		}}
	}
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"apply manager", conflict(`conflict with "kubectl"`), []string{"kubectl"}},
		{"update manager", conflict(`conflict with "kubectl-edit" using v1 at 2026-10-11T00:00:00Z`), []string{"kubectl-edit"}},
		{"subresource", conflict(`conflict with "helm" with subresource "status"`), []string{"helm"}},
		{"quote in name", conflict(`conflict with "file:a \"b\".yaml"`), []string{`file:a "b".yaml`}},
		{"dedup", conflict(`conflict with "kubectl"`, `conflict with "kubectl"`, `conflict with "api"`), []string{"kubectl", "api"}},
		{"unprefixed (older apiservers)", conflict(`"kubectl"`), []string{"kubectl"}},
		{"resourceVersion conflict", apierrors.NewConflict(gr, "home", errors.New("the object has been modified")), nil},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "kudurru.io", Kind: "DNSZone"}, "home", field.ErrorList{}), nil},
		{"plain error", errors.New("boom"), nil},
		{"wrapped", fmt.Errorf("apply: %w", conflict(`conflict with "kubectl"`)), []string{"kubectl"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := conflictingManagers(tt.err)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestApplyOptimisticConcurrencyWithSpecChange is the conformance case
// ApplyOptimisticConcurrency as proposed in dev-docs/spikes/store-kube.md: the
// second write changes the spec, so every driver must change the
// resourceVersion and bump generation. It runs against the kube driver and,
// to show the proposal holds for the reference driver too, against memory.
func TestApplyOptimisticConcurrencyWithSpecChange(t *testing.T) {
	run := func(t *testing.T, s store.Store) {
		ctx := context.Background()
		first, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			t.Fatal(err)
		}
		changed := zone("home", "home.example.")
		changed.Spec.DefaultTTL = 600
		second, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
		if err != nil {
			t.Fatal(err)
		}
		if second.GetResourceVersion() == first.GetResourceVersion() {
			t.Fatal("resourceVersion must change on every write that changes the object")
		}
		if second.GetGeneration() != 2 {
			t.Fatalf("generation should be 2, got %d", second.GetGeneration())
		}
		_, err = s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale resourceVersion must be ErrConflict, got %v", err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, memory.New()) })
	t.Run("kube", func(t *testing.T) {
		requireEnv(t)
		purge(t, shared.admin)
		run(t, newTestStore(t, Options{}))
	})
}

// TestApplyNoOpKeepsVersion pins the apiserver behaviour that makes the
// unchanged conformance case ApplyOptimisticConcurrency impossible: an
// identical re-apply is not a write, so neither resourceVersion nor
// generation moves.
func TestApplyNoOpKeepsVersion(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	first, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if second.GetResourceVersion() != first.GetResourceVersion() || second.GetGeneration() != 1 {
		t.Fatalf("no-op apply changed the object: rv %s -> %s, generation %d", first.GetResourceVersion(), second.GetResourceVersion(), second.GetGeneration())
	}
}

// TestApplyFieldConflictWithKubectl: fields written by another SSA manager
// (here a simulated `kubectl apply --server-side`) are protected; a
// conflicting kudurru write is ErrManagedBy naming that manager until Force.
func TestApplyFieldConflictWithKubectl(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "DNSZone",
		"metadata": map[string]any{"name": "lab"},
		"spec":     map[string]any{"zoneName": "lab.example.", "type": "primary", "defaultTTL": int64(120)},
	}}
	if err := shared.admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("kubectl")); err != nil {
		t.Fatal(err)
	}
	z := zone("lab", "lab.example.")
	z.Spec.DefaultTTL = 600
	_, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI})
	var mb *store.ErrManagedBy
	if !errors.As(err, &mb) || mb.Owner != "kubectl" {
		t.Fatalf("want ErrManagedBy{kubectl}, got %v", err)
	}
	out, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI, Force: true})
	if err != nil {
		t.Fatalf("Force: %v", err)
	}
	if got := out.(*v1alpha1.DNSZone); got.Spec.DefaultTTL != 600 || store.ManagedBy(got) != store.ManagerAPI {
		t.Fatalf("Force did not take over: ttl=%d managedBy=%q", got.Spec.DefaultTTL, store.ManagedBy(got))
	}
	// The same values as another manager are not a conflict: SSA shares
	// ownership, and the annotation records the kudurru writer.
	if _, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyLongManager(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	owner := store.ManagerFile("/etc/kudurru/" + strings.Repeat("deeply/nested/", 20) + "zones.yaml")
	out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: owner})
	if err != nil {
		t.Fatal(err)
	}
	if store.ManagedBy(out) != owner {
		t.Fatalf("annotation must hold the full manager, got %q", store.ManagedBy(out))
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: owner}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	var mb *store.ErrManagedBy
	if !errors.As(err, &mb) || mb.Owner != owner {
		t.Fatalf("want ErrManagedBy{%s}, got %v", owner, err)
	}
}

// TestApplyConcurrentFirstWriters: when several managers race to create the
// same object, exactly one wins and the rest get ErrManagedBy naming it.
func TestApplyConcurrentFirstWriters(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Apply(ctx, zone("race", "race.example."), store.ApplyOptions{Manager: store.ManagerIntegration(fmt.Sprint("w", i))})
		}()
	}
	wg.Wait()
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "race"}, &got); err != nil {
		t.Fatal(err)
	}
	winner := store.ManagedBy(&got)
	wins := 0
	for i, err := range errs {
		var mb *store.ErrManagedBy
		switch {
		case err == nil:
			wins++
			if store.ManagerIntegration(fmt.Sprint("w", i)) != winner {
				t.Errorf("writer w%d succeeded but the object is managed by %q", i, winner)
			}
		case errors.As(err, &mb):
			if mb.Owner != winner {
				t.Errorf("writer w%d: ErrManagedBy names %q, object is managed by %q", i, mb.Owner, winner)
			}
		default:
			t.Errorf("writer w%d: unexpected error %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d writers won, want exactly 1", wins)
	}
}

func TestApplyAndDeleteEdgeCases(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()

	_, err := s.Apply(ctx, zone("ghost", "ghost."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: "12345"})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("ExpectedResourceVersion on a missing object: want ErrConflict, got %v", err)
	}
	if _, err := s.Apply(ctx, rrset("", "noNS", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Error("namespaced kind without a namespace must be rejected")
	}
	z := zone("scoped", "scoped.")
	z.Namespace = "apps"
	if _, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Error("cluster-scoped kind with a namespace must be rejected")
	}
	if _, err := s.Apply(ctx, zone("bad", "not a zone name"), store.ApplyOptions{Manager: store.ManagerAPI}); !apierrors.IsInvalid(err) {
		t.Errorf("schema violation must surface the apiserver's Invalid error, got %v", err)
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var into v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Namespace: "apps", Name: "home"}, &into); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cluster-scoped Get with a namespace: want ErrNotFound, got %v", err)
	}
	var wrong v1alpha1.DNSRecordSet
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &wrong); err == nil {
		t.Error("Get into an object of another kind must fail")
	}
	if l, err := s.List(ctx, "DNSZone", store.ListOptions{Namespace: "apps"}); err != nil || len(l.Items) != 0 {
		t.Errorf("cluster-scoped List with a namespace: want empty, got %d (%v)", len(l.Items), err)
	}
	if _, err := s.List(ctx, "Bogus", store.ListOptions{}); err == nil {
		t.Error("unknown kind must fail")
	}
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: "file:x", Force: true}); err != nil {
		t.Errorf("Force delete by another manager: %v", err)
	}
}

// TestDeleteHonoursFinalizers: an object with a finalizer survives Delete
// with a deletionTimestamp; when its manager re-applies it without the
// finalizer, SSA removes the field and the apiserver completes the delete.
func TestDeleteHonoursFinalizers(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	z := zone("final", "final.example.")
	z.Finalizers = []string{"kudurru.io/test"}
	if _, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	key := store.Key{Kind: "DNSZone", Name: "final"}
	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, key, &got); err != nil {
		t.Fatalf("object with a finalizer must survive Delete: %v", err)
	}
	if got.DeletionTimestamp == nil {
		t.Fatal("deletionTimestamp not set")
	}
	if _, err := s.Apply(ctx, zone("final", "final.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "finalized object to disappear", func() bool {
		return errors.Is(s.Get(ctx, key, &got), store.ErrNotFound)
	})
}

func cleanupIntegration(name, policy string) *v1alpha1.Integration {
	return &v1alpha1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: v1alpha1.IntegrationSpec{
			Type:           "unifi",
			Credentials:    map[string]v1alpha1.CredentialReference{"apiKey": {SecretRef: &v1alpha1.CredentialSecretRef{Name: "unifi", Key: "token"}}},
			UniFi:          &v1alpha1.UniFiIntegration{Endpoint: "https://network.example"},
			DeletionPolicy: policy,
		},
	}
}

// finalizerOwners returns the main-resource field managers that own the
// cleanup finalizer of an Integration in the "apps" namespace.
func finalizerOwners(t *testing.T, name string) []string {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Integration"))
	if err := shared.admin.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: name}, u); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range u.GetManagedFields() {
		if e.Subresource == "" && e.FieldsV1 != nil && strings.Contains(e.FieldsV1.GetRawString(), store.IntegrationCleanupFinalizer) {
			out = append(out, e.Manager+"/"+string(e.Operation))
		}
	}
	return out
}

// TestReservedIntegrationFinalizerSurvivesApply: the apiserver has no
// kudurru admission, so the driver never arms the cleanup finalizer of a
// Delete-policy Integration itself; once the host armed it (D367), neither a
// manager's server-side apply nor a Force takeover removes it while the spec
// says Delete, and a Force takeover to Retain is the release.
func TestReservedIntegrationFinalizerSurvivesApply(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	const reserved = store.IntegrationCleanupFinalizer
	file := store.ManagerFile("integrations.yaml")
	key := store.Key{Kind: "Integration", Namespace: "apps", Name: "cleanup"}
	get := func() *v1alpha1.Integration {
		t.Helper()
		var got v1alpha1.Integration
		if err := s.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}

	claimed := cleanupIntegration("cleanup", "Delete")
	claimed.Finalizers = []string{reserved} // a document cannot arm it either
	created, err := s.Apply(ctx, claimed, store.ApplyOptions{Manager: file})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.GetFinalizers()) != 0 {
		t.Fatalf("Apply armed a finalizer nothing on this driver would clear: %v", created.GetFinalizers())
	}
	armed, err := s.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: reserved, ExpectedUID: string(created.GetUID()), ExpectedResourceVersion: created.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if !store.HasFinalizer(armed, reserved) || armed.GetResourceVersion() == created.GetResourceVersion() {
		t.Fatalf("arming: %+v", armed.GetFinalizers())
	}
	if owners := finalizerOwners(t, "cleanup"); len(owners) != 1 || owners[0] != finalizerFieldManager+"/Update" {
		t.Fatalf("the finalizer must be owned by the driver's finalizer manager alone: %v", owners)
	}

	// Non-Force server-side apply by the object's manager, without and with
	// a spec change: carried, never stripped.
	if _, err := s.Apply(ctx, cleanupIntegration("cleanup", "Delete"), store.ApplyOptions{Manager: file}); err != nil {
		t.Fatal(err)
	}
	edited := cleanupIntegration("cleanup", "Delete")
	edited.Spec.Suspend = true
	if _, err := s.Apply(ctx, edited, store.ApplyOptions{Manager: file}); err != nil {
		t.Fatal(err)
	}
	if got := get(); !store.HasFinalizer(got, reserved) || !got.Spec.Suspend {
		t.Fatalf("non-Force apply: finalizers=%v", got.Finalizers)
	}
	// A foreign field manager that never lists the finalizer (kubectl apply).
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Integration",
		"metadata": map[string]any{"name": "cleanup", "namespace": "apps", "labels": map[string]any{"owner": "ops"}},
	}}
	if err := shared.admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("kubectl")); err != nil {
		t.Fatal(err)
	}
	if got := get(); !store.HasFinalizer(got, reserved) || got.Labels["owner"] != "ops" {
		t.Fatalf("foreign apply: finalizers=%v labels=%v", got.Finalizers, got.Labels)
	}

	// Force is a whole-object takeover: the label and suspend go, the
	// cleanup obligation stays.
	if _, err := s.Apply(ctx, cleanupIntegration("cleanup", "Delete"), store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	taken := get()
	if !store.HasFinalizer(taken, reserved) || taken.Labels["owner"] != "" || taken.Spec.Suspend || store.ManagedBy(taken) != store.ManagerAPI {
		t.Fatalf("Force takeover: finalizers=%v labels=%v suspend=%v managedBy=%q", taken.Finalizers, taken.Labels, taken.Spec.Suspend, store.ManagedBy(taken))
	}
	if owners := finalizerOwners(t, "cleanup"); len(owners) != 1 || owners[0] != store.ManagerAPI+"/Apply" {
		t.Fatalf("after the takeover the new manager carries the finalizer: %v", owners)
	}
	// The new sole owner's ordinary apply still carries it.
	if _, err := s.Apply(ctx, cleanupIntegration("cleanup", "Delete"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if got := get(); !store.HasFinalizer(got, reserved) {
		t.Fatalf("apply after takeover stripped the finalizer: %v", got.Finalizers)
	}

	// Being deleted: applies, Force included, neither finalize nor revive.
	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		redeclared := cleanupIntegration("cleanup", "Delete")
		redeclared.Spec.Suspend = !force
		if _, err := s.Apply(ctx, redeclared, store.ApplyOptions{Manager: store.ManagerAPI, Force: force}); err != nil {
			t.Fatal(err)
		}
		if got := get(); got.DeletionTimestamp == nil || !store.HasFinalizer(got, reserved) || got.UID != created.GetUID() {
			t.Fatalf("apply during deletion (force=%v): deletionTimestamp=%v finalizers=%v", force, got.DeletionTimestamp, got.Finalizers)
		}
	}
	// The spec change to Retain by the manager that carries the finalizer
	// releases the object with no cleanup.
	if _, err := s.Apply(ctx, cleanupIntegration("cleanup", "Retain"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "released Integration to disappear", func() bool {
		return errors.Is(s.Get(ctx, key, &v1alpha1.Integration{}), store.ErrNotFound)
	})

	// Without a takeover, the host's own field ownership keeps the finalizer
	// through a manager's change to Retain; the host clears it.
	kept, err := s.Apply(ctx, cleanupIntegration("kept", "Delete"), store.ApplyOptions{Manager: file})
	if err != nil {
		t.Fatal(err)
	}
	keptKey := store.Key{Kind: "Integration", Namespace: "apps", Name: "kept"}
	if _, err := s.UpdateFinalizer(ctx, keptKey, store.FinalizerOptions{Finalizer: reserved, ExpectedUID: string(kept.GetUID()), ExpectedResourceVersion: kept.GetResourceVersion()}); err != nil {
		t.Fatal(err)
	}
	retained, err := s.Apply(ctx, cleanupIntegration("kept", "Retain"), store.ApplyOptions{Manager: file})
	if err != nil {
		t.Fatal(err)
	}
	if !store.HasFinalizer(retained, reserved) || retained.(*v1alpha1.Integration).Spec.DeletionPolicy != "Retain" {
		t.Fatalf("non-Force Retain: finalizers=%v", retained.GetFinalizers())
	}
	cleared, err := s.UpdateFinalizer(ctx, keptKey, store.FinalizerOptions{Finalizer: reserved, Remove: true, ExpectedUID: string(kept.GetUID()), ExpectedResourceVersion: retained.GetResourceVersion()})
	if err != nil || len(cleared.GetFinalizers()) != 0 || cleared.GetDeletionTimestamp() != nil {
		t.Fatalf("host disarm of a live object: %v %v", err, cleared)
	}
}

// TestUpdateFinalizerIsMetadataOnlyAndExact: the patch primitive changes
// metadata.finalizers alone, only on the exact UID and resourceVersion the
// caller observed, and the apiserver removes an object being deleted when
// its last finalizer goes.
func TestUpdateFinalizerIsMetadataOnlyAndExact(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	const reserved = store.IntegrationCleanupFinalizer
	file := store.ManagerFile("integrations.yaml")
	key := store.Key{Kind: "Integration", Namespace: "apps", Name: "exact"}
	labeled := cleanupIntegration("exact", "Delete")
	labeled.Labels = map[string]string{"team": "infra"}
	labeled.Annotations = map[string]string{"example.com/note": "kept"}
	created, err := s.Apply(ctx, labeled, store.ApplyOptions{Manager: file})
	if err != nil {
		t.Fatal(err)
	}
	withStatus := created.DeepCopyObject().(*v1alpha1.Integration)
	withStatus.Status.Clients = 7
	if err := s.UpdateStatus(ctx, withStatus); err != nil {
		t.Fatal(err)
	}
	var live v1alpha1.Integration
	if err := s.Get(ctx, key, &live); err != nil {
		t.Fatal(err)
	}
	opts := func(uid, rv string, remove bool, name string) store.FinalizerOptions {
		return store.FinalizerOptions{Finalizer: name, Remove: remove, ExpectedUID: uid, ExpectedResourceVersion: rv}
	}
	if _, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), created.GetResourceVersion(), false, reserved)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale resourceVersion: got %v, want ErrConflict", err)
	}
	if _, err := s.UpdateFinalizer(ctx, key, opts("another-incarnation", live.ResourceVersion, false, reserved)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong UID: got %v, want ErrConflict", err)
	}
	if _, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), "", false, reserved)); err == nil || errors.Is(err, store.ErrConflict) {
		t.Fatalf("missing resourceVersion: got %v, want a validation error", err)
	}
	if _, err := s.UpdateFinalizer(ctx, store.Key{Kind: "Integration", Namespace: "apps", Name: "absent"}, opts("uid", "1", false, reserved)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if _, err := s.UpdateFinalizer(ctx, store.Key{Kind: "Integration", Name: "exact"}, opts("uid", "1", false, reserved)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("key without the namespace of a namespaced kind: %v", err)
	}
	var unchanged v1alpha1.Integration
	if err := s.Get(ctx, key, &unchanged); err != nil || unchanged.ResourceVersion != live.ResourceVersion {
		t.Fatalf("a refused finalizer change wrote: %v", err)
	}

	armed, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), live.ResourceVersion, false, reserved))
	if err != nil {
		t.Fatal(err)
	}
	want := live.DeepCopy()
	want.Finalizers = []string{reserved}
	want.ResourceVersion = armed.GetResourceVersion()
	var stored v1alpha1.Integration
	if err := s.Get(ctx, key, &stored); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(&stored, want) || !equality.Semantic.DeepEqual(armed.(*v1alpha1.Integration), want) || armed.GetResourceVersion() == live.ResourceVersion {
		t.Fatalf("the patch changed more than metadata.finalizers:\n got %+v\nwant %+v", stored, *want)
	}
	// Already present: no request changes the object.
	again, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), armed.GetResourceVersion(), false, reserved))
	if err != nil || again.GetResourceVersion() != armed.GetResourceVersion() {
		t.Fatalf("no-op add: %v", err)
	}

	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: file}); err != nil {
		t.Fatal(err)
	}
	var deleting v1alpha1.Integration
	if err := s.Get(ctx, key, &deleting); err != nil || deleting.DeletionTimestamp == nil {
		t.Fatalf("armed object must survive Delete: %v", err)
	}
	if _, err := s.UpdateFinalizer(ctx, key, opts(string(deleting.UID), deleting.ResourceVersion, false, "kudurru.io/late")); !errors.Is(err, store.ErrTerminating) {
		t.Fatalf("adding during deletion: got %v, want ErrTerminating", err)
	}
	if _, err := s.UpdateFinalizer(ctx, key, opts(string(deleting.UID), armed.GetResourceVersion(), true, reserved)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale release: got %v, want ErrConflict", err)
	}
	removed, err := s.UpdateFinalizer(ctx, key, opts(string(deleting.UID), deleting.ResourceVersion, true, reserved))
	if err != nil || !store.Terminated(removed) || removed.(*v1alpha1.Integration).Status.Clients != 7 {
		t.Fatalf("release: %v %+v", err, removed)
	}
	eventually(t, 10*time.Second, "released Integration to disappear", func() bool {
		return errors.Is(s.Get(ctx, key, &v1alpha1.Integration{}), store.ErrNotFound)
	})
}

func TestUpdateStatusConcurrency(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	z := out.(*v1alpha1.DNSZone)
	z.Status.Serial = 1
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	z.Status.Serial = 2 // z still carries the pre-update resourceVersion
	if err := s.UpdateStatus(ctx, z); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale resourceVersion: want ErrConflict, got %v", err)
	}
	z.ResourceVersion = ""
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatalf("without a resourceVersion the write is unconditional: %v", err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.KeyOf(z), &got); err != nil || got.Status.Serial != 2 {
		t.Fatalf("serial=%d err=%v", got.Status.Serial, err)
	}
	ghost := zone("ghost", "ghost.")
	if err := s.UpdateStatus(ctx, ghost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing object: want ErrNotFound, got %v", err)
	}
}

func TestWatchNamespaceModifiedAndStop(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Apply(ctx, rrset("apps", "nas", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, rrset("dns", "ns1", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	w, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{Namespace: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	next := func() store.Event {
		t.Helper()
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				t.Fatal("watch closed early")
			}
			return ev
		case <-time.After(10 * time.Second):
			t.Fatal("no event within 10s")
		}
		return store.Event{}
	}
	if ev := next(); ev.Type != store.Added || ev.Object.GetName() != "nas" {
		t.Fatalf("want Added nas, got %s %s", ev.Type, ev.Object.GetName())
	}
	bm := next()
	if bm.Type != store.Bookmark || bm.Object == nil || bm.Object.GetResourceVersion() == "" {
		t.Fatalf("want a Bookmark carrying a resourceVersion, got %+v", bm)
	}
	if store.KindOf(bm.Object) != "DNSRecordSet" {
		t.Fatalf("bookmark object kind %s", store.KindOf(bm.Object))
	}
	// A write in another namespace is filtered out; the next event is the
	// spec change in "apps", then its status change.
	if _, err := s.Apply(ctx, rrset("dns", "ns2", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	changed := rrset("apps", "nas", map[string]string{"team": "infra"})
	out, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	ev := next()
	if ev.Type != store.Modified || ev.Object.GetLabels()["team"] != "infra" || len(ev.Object.GetManagedFields()) != 0 {
		t.Fatalf("want Modified nas with the label and no managedFields, got %s %v", ev.Type, ev.Object.GetLabels())
	}
	rs := out.(*v1alpha1.DNSRecordSet)
	rs.Status.Names = 1
	if err := s.UpdateStatus(ctx, rs); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Type != store.Modified || ev.Object.(*v1alpha1.DNSRecordSet).Status.Names != 1 {
		t.Fatalf("want Modified with status, got %s", ev.Type)
	}
	w.Stop()
	w.Stop() // idempotent
	for range w.ResultChan() {
	}

	// Cancelling the context ends a watch, and Close ends the rest.
	wctx, wcancel := context.WithCancel(ctx)
	w2, err := s.Watch(wctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wcancel()
	drained := make(chan struct{})
	go func() {
		for range w2.ResultChan() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not close after its context was cancelled")
	}
	w3, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range w3.ResultChan() {
	}
	if _, err := s.Watch(ctx, "DNSZone", store.WatchOptions{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Watch after Close: want ErrClosed, got %v", err)
	}
}

func TestClosedStoreReturnsErrClosed(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ctx := context.Background()
	var z v1alpha1.DNSZone
	checks := map[string]error{
		"Get":          s.Get(ctx, store.Key{Kind: "DNSZone", Name: "x"}, &z),
		"Delete":       s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "x"}, store.DeleteOptions{}),
		"UpdateStatus": s.UpdateStatus(ctx, zone("x", "x.")),
		"Snapshot":     s.Snapshot(ctx, &bytes.Buffer{}, store.SnapshotOptions{}),
		"Restore":      s.Restore(ctx, strings.NewReader("")),
	}
	_, checks["List"] = s.List(ctx, "DNSZone", store.ListOptions{})
	_, checks["Apply"] = s.Apply(ctx, zone("x", "x."), store.ApplyOptions{Manager: store.ManagerAPI})
	_, checks["Watch"] = s.Watch(ctx, "DNSZone", store.WatchOptions{})
	for op, err := range checks {
		if !errors.Is(err, store.ErrClosed) {
			t.Errorf("%s after Close: want ErrClosed, got %v", op, err)
		}
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()
	writes := []struct {
		obj     store.Object
		manager string
	}{
		{zone("home", "home.example."), store.ManagerAPI},
		{zone("lab", "lab.example."), store.ManagerFile("objects.d/zones.yaml")},
		{rrset("apps", "nas", map[string]string{"team": "infra"}), store.ManagerIntegration("unifi")},
		{rrset("dns", "ns1", nil), store.ManagerAPI},
		{&v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "kids"}}, store.ManagerAPI},
		{blocklist("apps", "ads"), store.ManagerBootstrap},
		{clientgroup("apps", "kids"), store.ManagerAPI},
	}
	for _, w := range writes {
		if _, err := s.Apply(ctx, w.obj, store.ApplyOptions{Manager: w.manager}); err != nil {
			t.Fatalf("apply %s: %v", store.KeyOf(w.obj), err)
		}
	}
	var buf bytes.Buffer
	if err := s.Snapshot(ctx, &buf, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIVersion string           `json:"apiVersion"`
		Kind       string           `json:"kind"`
		Items      []map[string]any `json:"items"`
	}
	if err := yaml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "List" || doc.APIVersion != "v1" || len(doc.Items) != len(writes) {
		t.Fatalf("snapshot: kind=%s apiVersion=%s items=%d", doc.Kind, doc.APIVersion, len(doc.Items))
	}
	if strings.Contains(buf.String(), "managedFields") {
		t.Fatal("snapshot must not carry managedFields")
	}
	for _, it := range doc.Items {
		if it["apiVersion"] != v1alpha1.GroupVersion.String() || it["kind"] == "" {
			t.Fatalf("item without TypeMeta: %v", it["metadata"])
		}
	}

	purge(t, shared.admin)
	if err := s.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	for _, w := range writes {
		key := store.KeyOf(w.obj)
		ki, _ := s.kinds.get(key.Kind)
		got := ki.newObject()
		if err := s.Get(ctx, key, got); err != nil {
			t.Fatalf("restored %s: %v", key, err)
		}
		if store.ManagedBy(got) != w.manager {
			t.Errorf("restored %s managed by %q, want %q", key, store.ManagedBy(got), w.manager)
		}
	}
	if err := s.Restore(ctx, strings.NewReader("apiVersion: kudurru.io/v1alpha1\nkind: Nope\nmetadata: {name: x}\n")); err == nil {
		t.Fatal("unknown kinds must be rejected")
	}
}

func TestInfo(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{})
	eventually(t, 10*time.Second, "cluster ID", func() bool { return s.Info().ClusterID != "" })
	info := s.Info()
	if info.Driver != DriverName || info.SchemaVersion != store.SchemaVersion {
		t.Fatalf("info = %+v", info)
	}
}

// TestApplyTooLarge: an object past store.MaxObjectBytes is refused by the
// apiserver (etcd's request cap) and surfaces as store.ErrTooLarge, the
// error the sqlite driver returns for the same object.
func TestApplyTooLarge(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{})
	entry := strings.Repeat("a", 60) + ".example.com"
	inline := make([]string, 0, store.MaxObjectBytes/len(entry)+1)
	for n := 0; n < store.MaxObjectBytes+64<<10; n += len(entry) + 3 {
		inline = append(inline, entry)
	}
	big := &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "apps"},
		Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: inline}}},
	}
	_, err := s.Apply(context.Background(), big, store.ApplyOptions{Manager: store.ManagerAPI})
	if !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("Apply past the cap: got %v, want ErrTooLarge", err)
	}
}

// TestWriteErrorTooLarge: the apiserver's 413 and etcd's "request is too
// large" (passed through as a 500) both wrap store.ErrTooLarge, as the
// sqlite driver reports objects past store.MaxObjectBytes.
func TestWriteErrorTooLarge(t *testing.T) {
	key := store.Key{Kind: "DNSZone", Name: "home"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"413", apierrors.NewRequestEntityTooLargeError("limit is 3145728"), true},
		{"etcd", apierrors.NewInternalError(errors.New("etcdserver: request is too large")), true},
		{"other", apierrors.NewBadRequest("invalid"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writeError("apply", key, tt.err)
			if got := errors.Is(err, store.ErrTooLarge); got != tt.want {
				t.Fatalf("errors.Is(%v, ErrTooLarge) = %v, want %v", err, got, tt.want)
			}
			if !errors.Is(err, tt.err) {
				t.Fatalf("%v does not wrap the apiserver error", err)
			}
		})
	}
}
