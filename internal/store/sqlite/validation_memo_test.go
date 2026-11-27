package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// The store remembers the images its replication validation accepted (D423):
// validating the same image again runs no owner validator, a refused image is
// refused on every attempt, another image is validated in full, and a
// remembered image is refused once the caller's context has ended.
func TestStoreRemembersOnlyTheImagesItsValidationAccepted(t *testing.T) {
	var calls atomic.Int64
	var refuse atomic.Bool
	s := openTemp(t, func(o *Options) {
		o.ValidateReplicaImage = func(*replication.Image) error {
			calls.Add(1)
			if refuse.Load() {
				return replication.ErrInvalid
			}
			return nil
		}
	})
	ctx := t.Context()
	image, err := s.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("the owner validator ran %d times for one image, want 1", calls.Load())
	}
	if a, b := first.Image(), second.Image(); !a.Generation().Same(b.Generation()) || !a.CreatedAt.Equal(b.CreatedAt) {
		t.Fatal("the remembered validation returned another image")
	}
	// A new read stamps another CreatedAt: another image, validated in full.
	again, err := s.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refuse.Store(true)
	for range 2 {
		if _, err := s.ValidateImage(ctx, *again); !errors.Is(err, replication.ErrInvalid) {
			t.Fatalf("a refused image was accepted: %v", err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("the owner validator ran %d times, want each refusal checked", calls.Load())
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ValidateImage(ended, *image); !errors.Is(err, context.Canceled) {
		t.Fatalf("a remembered image validated after its context ended: %v", err)
	}
}
