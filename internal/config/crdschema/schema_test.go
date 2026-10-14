package crdschema

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestSettingsAdmissionContainsOnlyPublicClusterKeys(t *testing.T) {
	s, err := Settings()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range config.Schema {
		field := s
		present := true
		for _, part := range strings.Split(key.Path, ".") {
			field, present = field.Properties[part]
			if !present {
				break
			}
		}
		if present != config.PublicClusterKey(key) {
			t.Fatalf("unexpected admission capability for %s", key.Path)
		}
		if present && field.Description != key.Help {
			t.Fatalf("help drift for %s", key.Path)
		}
	}
	data, err := os.ReadFile("../../../config/crd/bases/kudurru.io_kudurrus.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	actual := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["settings"]
	// JSON normalizes nil versus empty optional maps across YAML round trips.
	wantJSON, _ := json.Marshal(s)
	gotJSON, _ := json.Marshal(actual)
	if !reflect.DeepEqual(wantJSON, gotJSON) {
		t.Fatal("generated settings admission drift; run make generate")
	}
}
