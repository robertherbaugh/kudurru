package replication

import (
	"errors"
	"testing"
)

func TestCompleteOwnerValidationRunsAfterDigestAndCannotChangeImage(t *testing.T) {
	i := sample(t)
	opts := validation(i)
	denied := errors.New("incomplete owner inventory")
	calls := 0
	opts.ValidateImage = func(got *Image) error {
		calls++
		if !got.Generation().Same(i.Generation()) || len(got.Payload.Security) != 1 {
			t.Fatal("owner did not receive the complete verified cut")
		}
		return denied
	}
	if _, err := Validate(i, opts); err != denied || calls != 1 {
		t.Fatal("owner rejection lost", calls, err)
	}
	bad := i
	bad.PayloadSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := Validate(bad, opts); !errors.Is(err, ErrInvalid) || calls != 1 {
		t.Fatal("unverified payload reached owner", calls, err)
	}
	opts.ValidateImage = func(got *Image) error {
		got.Payload.Security[0].Data[0] = '!'
		got.KindManifest[0].Kind = "changed"
		got.Term.ClusterID = "changed"
		return nil
	}
	accepted, err := Validate(i, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(accepted.Image(), validation(i)); err != nil {
		t.Fatal("owner mutated accepted image", err)
	}
	if !accepted.Image().Generation().Same(i.Generation()) {
		t.Fatal("owner changed accepted generation")
	}
}
