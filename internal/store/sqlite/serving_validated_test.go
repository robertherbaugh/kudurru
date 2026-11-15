package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestValidatedServingCandidateRetainsOneCheckedCut(t *testing.T) {
	var objects, images int
	s := openTemp(t, func(o *Options) {
		o.ValidateReplicaObject = func(json.RawMessage) error { objects++; return nil }
		o.ValidateReplicaImage = func(*replication.Image) error { images++; return nil }
	})
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	validated, token, err := s.ReadValidatedServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	image := validated.Image()
	if objects != len(image.Payload.Objects) || images != 1 || !image.Generation().Same(token.Generation()) || !token.Matches(s.Authority()) {
		t.Fatal("validation and publication evidence did not describe one complete cut", objects, images)
	}
	image.Payload.Objects[0][0] = '!'
	if !json.Valid(validated.Image().Payload.Objects[0]) {
		t.Fatal("projection mutated retained validation")
	}
	mustApply(t, s, zone("new", "new.example."), store.ManagerAPI)
	called := false
	err = s.PublishIfCurrent(t.Context(), token, PublicationOptions{}, func() error { called = true; return nil })
	if !errors.Is(err, replication.ErrCAS) || called {
		t.Fatal("retained validation admitted an obsolete source", err)
	}
}

func TestValidatedServingCandidateRequiresValidationWithoutImageHook(t *testing.T) {
	want := errors.New("object owner refusal")
	s := openTemp(t, func(o *Options) {
		o.ValidateReplicaObject = func(json.RawMessage) error { return want }
	})
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	validated, token, err := s.ReadValidatedServingCandidate(t.Context())
	if !errors.Is(err, want) || validated != nil || token.source != nil {
		t.Fatal("missing optional image hook bypassed object validation", err)
	}
}

func TestValidatedServingCandidateRefusesCorruptionCancellationAndClosure(t *testing.T) {
	s := openTemp(t)
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	if _, err := s.w.ExecContext(t.Context(), `UPDATE objects SET data=json_set(data, '$.metadata.resourceVersion', '00')`); err != nil {
		t.Fatal(err)
	}
	if value, token, err := s.ReadValidatedServingCandidate(t.Context()); err == nil || value != nil || token.source != nil {
		t.Fatal("malformed installed object issued validated source", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if value, token, err := s.ReadValidatedServingCandidate(ctx); !errors.Is(err, context.Canceled) || value != nil || token.source != nil {
		t.Fatal("canceled read issued validated source", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if value, token, err := s.ReadValidatedServingCandidate(t.Context()); !errors.Is(err, store.ErrClosed) || value != nil || token.source != nil {
		t.Fatal("closed store issued validated source", err)
	}
}
