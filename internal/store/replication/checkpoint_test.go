package replication

import (
	"errors"
	"slices"
	"testing"
)

func TestRevocationCheckpointFeatureCannotBeOmitted(t *testing.T) {
	i := sample(t)
	opts := validation(i)
	i.RequiredFeatures = slices.DeleteFunc(slices.Clone(i.RequiredFeatures), func(f string) bool { return f == "auth-revocation-checkpoint-v1" })
	// Capabilities are negotiated independently of the payload digest.
	if digest, err := Digest(i); err != nil || digest != i.PayloadSHA256 {
		t.Fatal("fixture changed payload identity", err)
	}
	if _, err := Validate(i, opts); !errors.Is(err, ErrIncompatible) {
		t.Fatal("accepted sender without checkpoint semantics", err)
	}
}
