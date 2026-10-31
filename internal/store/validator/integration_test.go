package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestIntegrationCredentialAndProviderAdmission(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name   string
		change func(*v1alpha1.Integration)
		valid  bool
	}{
		{"valid secret reference", func(*v1alpha1.Integration) {}, true},
		{"missing credential", func(i *v1alpha1.Integration) { i.Spec.Credentials = nil }, false},
		{"empty reference", func(i *v1alpha1.Integration) { i.Spec.Credentials["apiToken"] = v1alpha1.CredentialReference{} }, false},
		{"two credential capabilities", func(i *v1alpha1.Integration) {
			c := i.Spec.Credentials["apiToken"]
			c.File = "/run/secrets/token"
			i.Spec.Credentials["apiToken"] = c
		}, false},
		{"empty secret key", func(i *v1alpha1.Integration) { i.Spec.Credentials["apiToken"].SecretRef.Key = "" }, false},
		{"provider mismatch", func(i *v1alpha1.Integration) { i.Spec.Type = "unifi" }, false},
		{"two providers", func(i *v1alpha1.Integration) {
			i.Spec.Webhook = &v1alpha1.WebhookIntegration{Endpoint: "http://localhost:8888"}
		}, false},
		{"duplicate destination", func(i *v1alpha1.Integration) {
			i.Spec.Cloudflare.Zones = append(i.Spec.Cloudflare.Zones, i.Spec.Cloudflare.Zones[0])
		}, false},
		{"no destination", func(i *v1alpha1.Integration) { i.Spec.Cloudflare.Zones = nil }, false},
		{"unknown plan policy", func(i *v1alpha1.Integration) { i.Spec.Cloudflare.Zones[0].Policy = "overwrite-all" }, false},
		{"unknown deletion policy", func(i *v1alpha1.Integration) { i.Spec.DeletionPolicy = "Erase" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := typedSample(t, "Integration").(*v1alpha1.Integration)
			tc.change(i)
			err := v.Validate(t.Context(), i, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestIntegrationDefaultsDisableExternalWrites(t *testing.T) {
	i := unstructuredFromYAML(t, `apiVersion: kudurru.io/v1alpha1
kind: Integration
metadata: {name: inventory, namespace: default}
spec:
  type: unifi
  credentials: {apiKey: {env: UNIFI_API_KEY}}
  unifi: {endpoint: "https://network.example"}
`)
	if err := testValidator(t).ValidateUnstructured(t.Context(), i, nil); err != nil {
		t.Fatal(err)
	}
	spec := i.Object["spec"].(map[string]any)
	if spec["deletionPolicy"] != "Retain" || spec["resync"] != "10m" {
		t.Fatal(spec)
	}
	unifi := spec["unifi"].(map[string]any)
	if _, ok := unifi["push"]; ok {
		t.Fatal("defaulted external writes", unifi)
	}
}
