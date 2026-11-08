package kube

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type beforeOwnerPatch struct {
	http.RoundTripper
	once   *atomic.Bool
	change func()
}

func (r beforeOwnerPatch) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.Method == http.MethodPatch && strings.HasSuffix(q.URL.Path, "/dnszones/created-race") && r.once.CompareAndSwap(false, true) {
		r.change()
	}
	return r.RoundTripper.RoundTrip(q)
}

func TestCreateOnlyOwnerMigrationRefusesForeignFieldsAndRace(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	for _, name := range []string{"created-foreign", "created-race"} {
		if _, err := s.Apply(t.Context(), zone(name, "created.example."), store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	foreign := func(name string) {
		t.Helper()
		obj := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := shared.admin.Patch(t.Context(), obj, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"foreign":"retained"}}}`)), client.FieldOwner("foreign-controller")); err != nil {
			t.Fatal(err)
		}
	}
	foreign("created-foreign")
	var old v1alpha1.DNSZone
	if err := s.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "created-foreign"}, &old); err != nil {
		t.Fatal(err)
	}
	old.Spec.DefaultTTL = 900
	if _, err := s.Apply(t.Context(), &old, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: old.ResourceVersion}); !store.IsManagedBy(err) {
		t.Fatal("foreign ownership was taken over", err)
	}
	var calls atomic.Bool
	cfg := rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return beforeOwnerPatch{rt, &calls, func() { foreign("created-race") }}
	})
	racing := newStoreOn(t, cfg, Options{})
	if err := racing.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "created-race"}, &old); err != nil {
		t.Fatal(err)
	}
	old.Spec.DefaultTTL = 901
	_, err := racing.Apply(context.Background(), &old, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: old.ResourceVersion})
	if !calls.Load() || (!errors.Is(err, store.ErrConflict) && !store.IsManagedBy(err)) {
		t.Fatal("concurrent foreign owner was rescued", err)
	}
	for _, name := range []string{"created-foreign", "created-race"} {
		var after v1alpha1.DNSZone
		if err := shared.admin.Get(t.Context(), client.ObjectKey{Name: name}, &after); err != nil {
			t.Fatal(err)
		}
		if after.Spec.DefaultTTL != 300 || after.Annotations["foreign"] != "retained" {
			t.Fatal("refusal changed original object", name)
		}
		found := false
		for _, owner := range after.ManagedFields {
			if owner.Manager == "foreign-controller" && owner.FieldsV1 != nil && strings.Contains(string(owner.FieldsV1.Raw), "foreign") {
				found = true
			}
		}
		if !found {
			t.Fatal("refusal transferred foreign managed fields", name)
		}
	}
}
