package validator

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateUnstructured(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name  string
		doc   string
		field string
		typ   metav1.CauseType
	}{
		{"unknown spec field is rejected (strict)", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: home}
spec: {zoneName: home.example., type: primary, ttl: 300}
`, "spec.ttl", CauseUnknownField},
		{"unknown top-level field is rejected", `
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: p, namespace: dns}
spec: {}
extra: true
`, "extra", CauseUnknownField},
		{"string where an integer belongs", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: home}
spec: {zoneName: home.example., type: primary, defaultTTL: "300"}
`, "spec.defaultTTL", typeInvalid},
		{"wrong version", `
apiVersion: kudurru.io/v1
kind: DNSZone
metadata: {name: home}
spec: {zoneName: home.example., type: primary}
`, "apiVersion", invalid},
		{"missing required zoneName", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: home}
spec: {type: primary}
`, "spec.zoneName", required},
		{"one-of via YAML", `
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: b, namespace: dns}
spec:
  sources: [{url: "https://a.example/", configMapRef: {name: lists}}]
`, "spec.sources[0]", invalid},
		{"malformed metadata", `
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: p, namespace: dns, labels: [a, b]}
spec: {}
`, "metadata", invalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := unstructuredFromYAML(t, tc.doc)
			wantInvalid(t, v.ValidateUnstructured(ctx(), u, nil), tc.field, tc.typ)
		})
	}

	t.Run("unknown kind", func(t *testing.T) {
		u := unstructuredFromYAML(t, "apiVersion: kudurru.io/v1alpha1\nkind: Zone\nmetadata: {name: x}\n")
		if err := v.ValidateUnstructured(ctx(), u, nil); !isUnknownKind(err) {
			t.Fatalf("want ErrUnknownKind, got %v", err)
		}
		u = unstructuredFromYAML(t, "apiVersion: example.com/v1alpha1\nkind: DNSZone\nmetadata: {name: x}\n")
		if err := v.ValidateUnstructured(ctx(), u, nil); !isUnknownKind(err) {
			t.Fatalf("want ErrUnknownKind for a foreign group, got %v", err)
		}
	})

	t.Run("transition via unstructured", func(t *testing.T) {
		old := unstructuredSample(t, "DNSZone")
		upd := unstructuredSample(t, "DNSZone")
		if err := unstructuredSet(upd.Object, "lab.example.com.", "spec", "zoneName"); err != nil {
			t.Fatal(err)
		}
		wantInvalid(t, v.ValidateUnstructured(ctx(), upd, old), "spec.zoneName", invalid)
	})

	t.Run("rejected input is left unchanged", func(t *testing.T) {
		u := unstructuredFromYAML(t, "apiVersion: kudurru.io/v1alpha1\nkind: DNSZone\nmetadata: {name: home}\nspec: {zoneName: home.example., type: master, soa: {}}\n")
		before := u.DeepCopy()
		if err := v.ValidateUnstructured(ctx(), u, nil); err == nil {
			t.Fatal("accepted")
		}
		if !equalJSON(t, before.Object, u.Object) {
			t.Fatal("input was modified")
		}
	})
}

func unstructuredSet(obj map[string]any, value any, fields ...string) error {
	m := obj
	for _, f := range fields[:len(fields)-1] {
		m = m[f].(map[string]any)
	}
	m[fields[len(fields)-1]] = value
	return nil
}
