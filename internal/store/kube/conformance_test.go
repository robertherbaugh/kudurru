package kube

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/conformance"
)

// TestConformance runs the driver conformance suite against a real
// kube-apiserver (envtest). Each case gets an empty control plane and a
// fresh driver.
func TestConformance(t *testing.T) {
	requireEnv(t)
	conformance.Run(t, func(t *testing.T) store.Store {
		purge(t, shared.admin)
		return newTestStore(t, Options{})
	})
}

func TestRejectionConformance(t *testing.T) {
	requireEnv(t)
	conformance.RunRejections(t, func(t *testing.T) store.Store {
		purge(t, shared.admin)
		return newTestStore(t, Options{})
	}, func(t *testing.T, _ store.Store, key store.Key) any {
		// Store.Get/List intentionally strip managedFields. Check the actual
		// apiserver metadata too, so rejected Force cannot conceal SSA takeover.
		meta := &metav1.PartialObjectMetadata{}
		meta.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(string(key.Kind)))
		err := shared.admin.Get(t.Context(), client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, meta)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return meta
	})
}
