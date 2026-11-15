package kube

import (
	"encoding/json"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// These exercise installed CRDs, independently of the embedded SQLite schema.
func TestNotifyPolicyActualCRD(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{})
	z := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "notify-contract"},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "notify-contract.test.", Type: v1alpha1.ZoneTypePrimary,
			Policy: v1alpha1.ZonePolicy{NotifyTSIGKeyRef: "separate.example.", NotifySource: "::1"}},
	}
	if _, err := s.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	key := store.Key{Kind: "DNSZone", Name: z.Name}
	var saved v1alpha1.DNSZone
	if err := s.Get(t.Context(), key, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Spec.Policy.NotifyTSIGKeyRef != z.Spec.Policy.NotifyTSIGKeyRef || saved.Spec.Policy.NotifySource != "::1" {
		t.Fatal("apiserver pruned or replaced the explicit NOTIFY choice")
	}
	for _, change := range []func(*v1alpha1.DNSZone){
		func(z *v1alpha1.DNSZone) { z.Spec.Policy.NotifyTSIGKeyRef = "bad..key" },
		func(z *v1alpha1.DNSZone) { z.Spec.Policy.NotifySource = "::ffff:127.0.0.1" },
		func(z *v1alpha1.DNSZone) { z.Spec.Policy.NotifySource = "ff02::1" },
	} {
		invalid := saved.DeepCopy()
		change(invalid)
		if err := shared.admin.Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("direct apiserver update did not reject invalid policy: %v", err)
		}
	}
	var after v1alpha1.DNSZone
	if err := s.Get(t.Context(), key, &after); err != nil || after.ResourceVersion != saved.ResourceVersion || after.Spec.Policy.NotifySource != "::1" || after.Spec.Policy.NotifyTSIGKeyRef != saved.Spec.Policy.NotifyTSIGKeyRef {
		t.Fatal("refused NOTIFY edits changed the stored object")
	}
}

func TestRecursiveNTAActualCRD(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	u := config.Upstreams{Recursion: config.Recursion{NegativeTrustAnchors: []config.RecursiveNegativeTrustAnchor{{Domain: "broken.example.", Reason: "incident", ExpiresAt: expiry}}}}
	raw, err := json.Marshal(map[string]any{"recursion": map[string]any{"negativeTrustAnchors": u.Recursion.NegativeTrustAnchors}})
	if err != nil {
		t.Fatal(err)
	}
	obj := &v1alpha1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.KudurruSpec{
		Settings: map[string]apiextensionsv1.JSON{"upstreams": {Raw: raw}},
	}}
	if _, err := s.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var saved v1alpha1.Kudurru
	key := store.Key{Kind: "Kudurru", Name: "default"}
	if err := s.Get(t.Context(), key, &saved); err != nil {
		t.Fatal(err)
	}
	var decoded config.Upstreams
	if err := json.Unmarshal(saved.Spec.Settings["upstreams"].Raw, &decoded); err != nil {
		t.Fatal(err)
	}
	ntas := decoded.Recursion.NegativeTrustAnchors
	if len(ntas) != 1 || ntas[0].Domain != "broken.example." || ntas[0].Reason != "incident" || !ntas[0].ExpiresAt.Equal(expiry) {
		t.Fatal("apiserver pruned or renewed the recursive exception")
	}
	invalid := saved.DeepCopy()
	invalid.Spec.Settings["upstreams"] = apiextensionsv1.JSON{Raw: []byte(`{"recursion":{"negativeTrustAnchors":[{"domain":"broken.example.","reason":"incident","expiresAt":true}]}}`)}
	if err := shared.admin.Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
		t.Fatalf("direct apiserver update accepted invalid exception shape: %v", err)
	}
	var after v1alpha1.Kudurru
	if err := s.Get(t.Context(), key, &after); err != nil || after.ResourceVersion != saved.ResourceVersion {
		t.Fatal("refused exception edit changed the stored object")
	}
}
