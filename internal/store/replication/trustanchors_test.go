package replication

import (
	"errors"
	"slices"
	"testing"
)

func TestManagedRootAnchorsFeatureAndSchemaAreIndependentGates(t *testing.T) {
	oldFeatures := []string{"complete-set-v1", "singleton-settings-v1", "opaque-sections-v1", "provider-registry-v1", "inventory-state-v1", "auth-browser-attempts-v1"}
	if !slices.Equal(FeatureNames(), append(slices.Clone(oldFeatures), "managed-root-anchors-v1", "managed-root-runtime-v1", "member-enrollment-v1")) {
		t.Fatal("unexpected required feature contract")
	}
	for _, stage := range []string{"current", "old-schema", "schema6-receiver", "old-features", "both-old", "unknown", "duplicate"} {
		t.Run(stage, func(t *testing.T) {
			i := sample(t)
			i.StoreSchemaVersion = 7
			opts := validation(i)
			opts.SchemaVersion = 7
			switch stage {
			case "old-schema":
				i.StoreSchemaVersion = 6
			case "schema6-receiver":
				opts.SchemaVersion = 6
			case "old-features":
				i.RequiredFeatures = slices.Clone(oldFeatures)
			case "both-old":
				i.StoreSchemaVersion, i.RequiredFeatures = 6, slices.Clone(oldFeatures)
			case "unknown":
				i.RequiredFeatures = append(slices.Clone(oldFeatures), "future-anchor-owner-v2")
			case "duplicate":
				i.RequiredFeatures = append(slices.Clone(i.RequiredFeatures), "managed-root-anchors-v1")
			}
			// Schema/features are outside the digest. A matching payload ETag
			// must not bypass negotiation; do not reseal the input to repair it.
			digest, err := Digest(i)
			if err != nil || digest != i.PayloadSHA256 {
				t.Fatal("fixture changed payload identity", err)
			}
			_, err = Validate(i, opts)
			want := ErrIncompatible
			if stage == "current" {
				want = nil
			}
			if stage == "duplicate" {
				want = ErrInvalid
			}
			if !errors.Is(err, want) {
				t.Fatal("unexpected schema/feature admission", err)
			}
		})
	}
}

func TestManagedRootRuntimeFeatureAndSchemaAreIndependentGates(t *testing.T) {
	for _, stage := range []string{"current", "schema7-image", "schema7-receiver", "codec-only", "missing-owner-feature", "unknown", "duplicate"} {
		t.Run(stage, func(t *testing.T) {
			i := sample(t)
			i.StoreSchemaVersion = 8
			opts := validation(i)
			opts.SchemaVersion = 8
			switch stage {
			case "schema7-image":
				i.StoreSchemaVersion = 7
			case "schema7-receiver":
				opts.SchemaVersion = 7
			case "codec-only":
				i.RequiredFeatures = slices.DeleteFunc(slices.Clone(i.RequiredFeatures), func(s string) bool { return s == "managed-root-runtime-v1" })
			case "missing-owner-feature":
				i.RequiredFeatures = slices.DeleteFunc(slices.Clone(i.RequiredFeatures), func(s string) bool { return s == "managed-root-anchors-v1" })
			case "unknown":
				i.RequiredFeatures = append(slices.Clone(i.RequiredFeatures), "future-root-runtime-v2")
			case "duplicate":
				i.RequiredFeatures = append(slices.Clone(i.RequiredFeatures), "managed-root-runtime-v1")
			}
			// A matching payload digest cannot replace capability negotiation.
			digest, err := Digest(i)
			if err != nil || digest != i.PayloadSHA256 {
				t.Fatal("fixture changed complete payload identity", err)
			}
			_, err = Validate(i, opts)
			want := ErrIncompatible
			if stage == "current" {
				want = nil
			}
			if stage == "duplicate" {
				want = ErrInvalid
			}
			if !errors.Is(err, want) {
				t.Fatal("schema8 runtime capability gate changed", err)
			}
		})
	}
}
