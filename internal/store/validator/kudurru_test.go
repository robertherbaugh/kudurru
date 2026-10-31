package validator

import "testing"

func TestKudurruSettingsAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		valid          bool
	}{
		{"public values", "cache: {maxEntries: 1234}\n    response: {filterAAAA: true}", true},
		{"host-only audit", "audit: {store: local}", false},
		{"host-only identity capability", "auth: {identityProviders: []}", false},
		{"node listener", "listeners: {api: {address: ':9999'}}", false},
		{"unknown setting", "cache: {enabled: false}", false},
		{"wrong type", "cache: {maxEntries: true}", false},
		{"invalid enum", "blocking: {defaultResponse: erase}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := unstructuredFromYAML(t, "apiVersion: kudurru.io/v1alpha1\nkind: Kudurru\nmetadata: {name: default}\nspec:\n  settings:\n    "+tc.settings+"\n")
			err := testValidator(t).ValidateUnstructured(t.Context(), u, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	u := unstructuredSample(t, "Kudurru")
	u.SetName("another-cluster-config")
	if err := testValidator(t).ValidateUnstructured(t.Context(), u, nil); err == nil {
		t.Fatal("second singleton name accepted")
	}
}
