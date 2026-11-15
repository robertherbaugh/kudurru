package replication

import (
	"errors"
	"slices"
	"testing"
)

func TestEnrollmentSchemaAndFeatureRefuseOldPeersIndependently(t *testing.T) {
	for _, mode := range []string{"current", "schema8-input", "schema8-receiver", "missing-feature", "unknown-feature", "duplicate-feature"} {
		t.Run(mode, func(t *testing.T) {
			i := sample(t)
			i.StoreSchemaVersion = 9
			opts := validation(i)
			opts.SchemaVersion = 9
			switch mode {
			case "schema8-input":
				i.StoreSchemaVersion = 8
			case "schema8-receiver":
				opts.SchemaVersion = 8
			case "missing-feature":
				i.RequiredFeatures = slices.DeleteFunc(slices.Clone(i.RequiredFeatures), func(f string) bool { return f == "member-enrollment-v1" })
			case "unknown-feature":
				i.RequiredFeatures = append(slices.Clone(i.RequiredFeatures), "member-enrollment-v2")
			case "duplicate-feature":
				i.RequiredFeatures = append(slices.Clone(i.RequiredFeatures), "member-enrollment-v1")
			}
			digest, err := Digest(i)
			if err != nil || digest != i.PayloadSHA256 {
				t.Fatal("negotiation changed payload fixture", err)
			}
			_, err = Validate(i, opts)
			if mode == "current" {
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "duplicate-feature" {
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("duplicate feature accepted", err)
				}
			} else if !errors.Is(err, ErrIncompatible) {
				t.Fatal("incompatible enrollment peer accepted", err)
			}
		})
	}
}
