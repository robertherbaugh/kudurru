package kube

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// finalizerPatchTransport runs change once, immediately before the first
// finalizer patch of the named Integration reaches the apiserver: after the
// driver's read and its client-side identity check, before its write.
type finalizerPatchTransport struct {
	http.RoundTripper
	name   string
	calls  *atomic.Int32
	change func()
}

func (d finalizerPatchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/integrations/"+d.name) && r.URL.Query().Get("fieldManager") == finalizerFieldManager {
		if d.calls.Add(1) == 1 {
			d.change()
		}
	}
	return d.RoundTripper.RoundTrip(r)
}

// TestUpdateFinalizerActualConflictIsNotRetried proves the apiserver-side
// half of the UpdateFinalizer compare-and-swap (D367). The merge patch
// replaces the whole finalizers list, so the resourceVersion the driver read
// must travel in the patch: an object that changes between the driver's read
// and its write is store.ErrConflict, the patch is sent exactly once, and a
// finalizer another actor added in between survives. The client-side
// comparison alone (TestUpdateFinalizerIsMetadataOnlyAndExact) cannot see
// that change.
func TestUpdateFinalizerActualConflictIsNotRetried(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	const other = "example.com/other"
	for _, tc := range []struct {
		name string
		// remove selects releasing the reserved finalizer (the cleanup owner's
		// write) instead of arming it (the host's write).
		remove bool
		// concurrent is the list another actor writes between read and patch.
		concurrent string
		want       []string
	}{
		{name: "finalizer-race-add", concurrent: `["` + other + `"]`, want: []string{other}},
		{name: "finalizer-race-remove", remove: true, concurrent: `["` + store.IntegrationCleanupFinalizer + `","` + other + `"]`, want: []string{store.IntegrationCleanupFinalizer, other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			cfg := rest.CopyConfig(shared.cfg)
			cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
				return finalizerPatchTransport{rt, tc.name, &calls, func() {
					obj := &v1alpha1.Integration{}
					obj.Namespace, obj.Name = "apps", tc.name
					patch := []byte(`{"metadata":{"finalizers":` + tc.concurrent + `}}`)
					if err := shared.admin.Patch(t.Context(), obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
						t.Error(err)
					}
				}}
			})
			s := newStoreOn(t, cfg, Options{})
			key := store.Key{Kind: "Integration", Namespace: "apps", Name: tc.name}
			observed, err := s.Apply(t.Context(), cleanupIntegration(tc.name, "Delete"), store.ApplyOptions{Manager: store.ManagerAPI})
			if err != nil {
				t.Fatal(err)
			}
			opts := store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, ExpectedUID: string(observed.GetUID()), ExpectedResourceVersion: observed.GetResourceVersion()}
			if tc.remove {
				// Arm outside the driver, so the hook sees only the release.
				arm := &v1alpha1.Integration{}
				arm.Namespace, arm.Name = "apps", tc.name
				if err := shared.admin.Patch(t.Context(), arm, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":["`+store.IntegrationCleanupFinalizer+`"]}}`))); err != nil {
					t.Fatal(err)
				}
				var armed v1alpha1.Integration
				if err := s.Get(t.Context(), key, &armed); err != nil || !store.HasFinalizer(&armed, store.IntegrationCleanupFinalizer) {
					t.Fatalf("arming the fixture: %v %v", err, armed.Finalizers)
				}
				observed = &armed
				opts.Remove, opts.ExpectedResourceVersion = true, armed.ResourceVersion
			}
			_, err = s.UpdateFinalizer(t.Context(), key, opts)
			if !errors.Is(err, store.ErrConflict) || calls.Load() != 1 {
				t.Fatalf("a change between the read and the patch: err=%v patches=%d, want ErrConflict after exactly one patch", err, calls.Load())
			}
			var after v1alpha1.Integration
			if err := s.Get(t.Context(), key, &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Finalizers, tc.want) || after.UID != observed.GetUID() || after.ResourceVersion == observed.GetResourceVersion() {
				t.Fatalf("the stale finalizer write landed or the concurrent change was lost: finalizers=%v, want %v", after.Finalizers, tc.want)
			}
		})
	}
}
