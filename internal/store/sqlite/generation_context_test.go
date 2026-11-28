package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// A whole-generation read whose context ends after its queries, while the
// seal and size check are still to run, returns the context's error and no
// image. It never answers late as if it had read in time (#281).
func TestReadGenerationStopsWhenItsContextEndsAfterTheQueries(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Apply(t.Context(), zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.generationRows = cancel
	image, err := s.ReadGeneration(ctx)
	s.generationRows = nil
	if !errors.Is(err, context.Canceled) || image != nil {
		t.Fatalf("read past its context returned an image: %t, error %v; want the context's error and no image", image != nil, err)
	}
	if image := imageOf(t, s); len(image.Payload.Objects) != 1 || len(image.PayloadSHA256) != 64 {
		t.Fatal("a canceled read changed the next one")
	}
}

// A changing command whose context ends during its whole-generation
// postimage read refuses with that error and commits nothing: no object
// change, no new revision and no generation notification.
func TestCommandWhoseContextEndsInThePostimageCommitsNothing(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Apply(t.Context(), zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	before := imageOf(t, s)
	changes := s.GenerationChanges()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var armed, reached atomic.Bool
	s.generationRows = func() {
		if armed.Load() {
			reached.Store(true)
			cancel()
		}
	}
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		z := zone("home", "home.example.")
		z.Labels = map[string]string{"changed": "yes"}
		if _, err := tx.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		armed.Store(true)
		return nil
	})
	s.generationRows = nil
	if !reached.Load() {
		t.Fatal("the command did not read its postimage")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("command canceled in its postimage returned %v, want the context's error", err)
	}
	after := imageOf(t, s)
	if after.Revision != before.Revision || after.ObjectResourceVersion != before.ObjectResourceVersion || after.PayloadSHA256 != before.PayloadSHA256 {
		t.Fatalf("a canceled command committed: revision %d -> %d, objects %d -> %d", before.Revision, after.Revision, before.ObjectResourceVersion, after.ObjectResourceVersion)
	}
	select {
	case <-changes:
		t.Fatal("a canceled command published a generation change")
	default:
	}
	var stored v1alpha1.DNSZone
	if err := s.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, &stored); err != nil || stored.Labels["changed"] != "" {
		t.Fatalf("a canceled command changed the object: %v, %v", err, stored.Labels)
	}
}
