package main

import (
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/license"
)

func TestParseFeatures(t *testing.T) {
	got, err := parseFeatures(" rbac, oidc ,rbac,", false)
	if err != nil || len(got) != 2 || got[0] != license.FeatureRBAC || got[1] != license.FeatureOIDC {
		t.Fatalf("parseFeatures = %v, %v", got, err)
	}
	if got, err := parseFeatures("", false); err != nil || got != nil {
		t.Errorf("an empty list (every feature) = %v, %v", got, err)
	}
	// Every feature this build knows is accepted, so the flag cannot lag behind the library.
	if _, err := parseFeatures(featureNames(), false); err != nil {
		t.Errorf("the known features were refused: %v", err)
	}
	for _, f := range license.Features() {
		if !strings.Contains(featureNames(), string(f)) {
			t.Errorf("the help text lacks %s", f)
		}
	}
}

func TestParseFeaturesRefusesATypo(t *testing.T) {
	_, err := parseFeatures("oidc,rback", false)
	if err == nil || !strings.Contains(err.Error(), `unknown feature "rback"`) || !strings.Contains(err.Error(), "rbac") {
		t.Fatalf("a typo was accepted or the error is unhelpful: %v", err)
	}
	got, err := parseFeatures("oidc,from-the-future", true)
	if err != nil || len(got) != 2 || got[1] != "from-the-future" {
		t.Errorf("-allow-unknown-features = %v, %v", got, err)
	}
}
