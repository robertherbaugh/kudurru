package kube

import (
	"encoding/json"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Installed schemas must preserve both lists before the settings subscriber
// can compile a complete control view. Test the actual apiserver, not just the
// embedded validator used by SQLite.
func TestControlActualCRD(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	want := config.Control{
		AllowedNetworks: []string{"192.0.2.0/24", "2001:db8::/32"},
		BlockedQTypes:   []string{"ANY", "TYPE65280"},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	obj := &v1alpha1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.KudurruSpec{
		Settings: map[string]apiextensionsv1.JSON{"control": {Raw: raw}},
	}}
	if _, err := s.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	key := store.Key{Kind: "Kudurru", Name: "default"}
	var saved v1alpha1.Kudurru
	if err := s.Get(t.Context(), key, &saved); err != nil {
		t.Fatal(err)
	}
	var got config.Control
	if err := json.Unmarshal(saved.Spec.Settings["control"].Raw, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("apiserver pruned or changed global DNS controls")
	}
	if _, err := got.Rules(); err != nil {
		t.Fatal(err)
	}
	for _, invalidRaw := range []string{
		`{"allowedNetworks":[true],"blockedQTypes":["A"]}`,
		`{"allowedNetworks":["192.0.2.0/24"],"blockedQTypes":[1]}`,
		`{"allowedNetworks":"192.0.2.0/24","blockedQTypes":["A"]}`,
	} {
		invalid := saved.DeepCopy()
		invalid.Spec.Settings["control"] = apiextensionsv1.JSON{Raw: []byte(invalidRaw)}
		if err := shared.admin.Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("direct apiserver update accepted invalid control shape: %v", err)
		}
	}
	var after v1alpha1.Kudurru
	if err := s.Get(t.Context(), key, &after); err != nil || after.ResourceVersion != saved.ResourceVersion {
		t.Fatal("refused control edits changed the stored object")
	}
}
