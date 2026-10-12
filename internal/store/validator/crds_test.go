package validator

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// TestEmbeddedCRDsMatchManifests fails when internal/store/validator/crds
// drifts from config/crd/bases, the output of `make manifests`. Fix it with
// `make manifests` (which copies the CRDs here), never by hand.
func TestEmbeddedCRDsMatchManifests(t *testing.T) {
	embedded, err := EmbeddedCRDs()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(repoRoot, "config", "crd", "bases", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, p := range paths {
		name := filepath.Base(p)
		onDisk = append(onDisk, name)
		want, err := os.ReadFile(p) //nolint:gosec // paths come from a glob of config/crd/bases
		if err != nil {
			t.Fatal(err)
		}
		got, ok := embedded[name]
		if !ok {
			t.Errorf("%s is in config/crd/bases but not embedded; run `make manifests`", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("embedded %s differs from config/crd/bases; run `make manifests`", name)
		}
	}
	var names []string
	for n := range embedded {
		names = append(names, n)
	}
	sort.Strings(names)
	sort.Strings(onDisk)
	if strings.Join(names, ",") != strings.Join(onDisk, ",") {
		t.Errorf("embedded CRDs %v, config/crd/bases %v; run `make manifests`", names, onDisk)
	}
	if len(names) != len(sampleFiles) {
		t.Errorf("%d CRDs embedded, %d sample kinds: add a sample (and test cases) for every kind", len(names), len(sampleFiles))
	}
}

// TestEmbeddedCRDsInstall runs the apiserver's own CustomResourceDefinition
// create validation over every embedded CRD: structural schema, pruned
// defaults, list-type rules and the static CEL cost estimate. A CRD that
// fails here would be refused by `kubectl apply` on a real cluster.
func TestEmbeddedCRDsInstall(t *testing.T) {
	embedded, err := EmbeddedCRDs()
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range embedded {
		t.Run(name, func(t *testing.T) {
			if errs := crdInstallErrors(t, doc); len(errs) > 0 {
				t.Fatalf("the apiserver would refuse this CRD:\n%v", errs.ToAggregate())
			}
		})
	}
}

// crdInstallErrors is what the apiserver's CRD registry returns for a create.
func crdInstallErrors(t *testing.T, doc []byte) field.ErrorList {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(doc, &crd); err != nil {
		t.Fatal(err)
	}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(&crd)
	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internal, nil); err != nil {
		t.Fatal(err)
	}
	// customresourcedefinition strategy.PrepareForCreate.
	internal.Status = apiextensions.CustomResourceDefinitionStatus{}
	for _, v := range internal.Spec.Versions {
		if v.Storage {
			internal.Status.StoredVersions = []string{v.Name}
		}
	}
	return crdvalidation.ValidateCustomResourceDefinition(ctx(), &internal)
}

func TestNewFromCRDsRejectsBadInput(t *testing.T) {
	embedded, err := EmbeddedCRDs()
	if err != nil {
		t.Fatal(err)
	}
	zone := embedded["kudurru.io_dnszones.yaml"]
	cases := map[string][][]byte{
		"not yaml":      {[]byte("{")},
		"not a CRD":     {[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n")},
		"duplicate":     {zone, zone},
		"unknown field": {append(append([]byte{}, zone...), []byte("bogus: 1\n")...)},
	}
	for name, docs := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFromCRDs(docs...); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	v, err := NewFromCRDs(zone)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Kinds()) != 1 {
		t.Fatalf("kinds = %v", v.Kinds())
	}
}
