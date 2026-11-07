package validator

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPruneApplyNullsPreservesNullableAndDefaultedValues(t *testing.T) {
	v, err := NewFromCRDs([]byte(`
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: nullfixtures.kudurru.io}
spec:
  group: kudurru.io
  names: {kind: NullFixture, plural: nullfixtures}
  scope: Cluster
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              requiredValues: {type: array, items: {type: string}}
              nullableValue: {type: string, nullable: true}
              defaultedValue: {type: string, default: fallback}
              children: {type: object, additionalProperties: {type: string}}
              values: {type: array, items: {type: string}}
`))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "NullFixture",
		"spec": map[string]any{
			"requiredValues": nil, "nullableValue": nil, "defaultedValue": nil,
			"children": map[string]any{"empty": nil, "kept": "value"},
			"values":   []any{nil, "kept"},
		},
	}}
	if err := v.PruneApplyNulls(u); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"nullableValue": nil, "defaultedValue": nil,
		"children": map[string]any{"kept": "value"}, "values": []any{nil, "kept"},
	}
	if !reflect.DeepEqual(u.Object["spec"], want) {
		t.Fatalf("pruning lost nullable/default/list semantics: %+v", u.Object["spec"])
	}
	// A foreign version cannot accidentally use a different schema. Rejection
	// also leaves the payload alone.
	u.SetAPIVersion("kudurru.io/v2")
	before := u.DeepCopy()
	if err := v.PruneApplyNulls(u); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("foreign version: %v", err)
	}
	if !reflect.DeepEqual(before, u) {
		t.Fatal("rejected normalization changed payload")
	}
	if err := v.PruneApplyNulls(nil); err == nil {
		t.Fatal("nil payload was accepted")
	}
}
