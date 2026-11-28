package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// contextImage is a sealed image of n DNSZone objects.
func contextImage(t *testing.T, n int) Image {
	t.Helper()
	i := sample(t)
	i.Payload.Objects = make([]json.RawMessage, 0, n)
	for k := range n {
		i.Payload.Objects = append(i.Payload.Objects, json.RawMessage(fmt.Sprintf(`{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"zone-%06d","resourceVersion":"9"},"spec":{}}`, k)))
	}
	if err := Seal(&i); err != nil {
		t.Fatal(err)
	}
	return i
}

// The work between objects is slowed to 1 ms each, so the whole loop takes
// two seconds. Its operation has 100 ms: it must stop with that deadline at
// about 100 ms (#281), not finish the loop and answer late.
const (
	contextObjects  = 2000
	contextStep     = time.Millisecond
	contextDeadline = 100 * time.Millisecond
	// contextPrompt leaves room for a slow, shared runner and the race
	// detector while staying far below the two seconds of the whole loop.
	contextPrompt = time.Second
)

func TestSealStopsAtItsContextDeadline(t *testing.T) {
	i := contextImage(t, contextObjects)
	var steps atomic.Int64
	objectStep = func() {
		steps.Add(1)
		time.Sleep(contextStep)
	}
	t.Cleanup(func() { objectStep = nil })
	ctx, cancel := context.WithTimeout(t.Context(), contextDeadline)
	defer cancel()
	started := time.Now()
	err := SealContext(ctx, &i)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("seal past its deadline returned %v after %s, want the deadline", err, elapsed)
	}
	if elapsed > contextPrompt || steps.Load() >= contextObjects {
		t.Fatalf("seal stopped after %s and %d of %d objects, want a prompt stop", elapsed, steps.Load(), contextObjects)
	}
	if i.PayloadSHA256 != "" {
		t.Fatal("a canceled seal left a digest, so it reads as complete")
	}
	objectStep = nil
	if err := SealContext(t.Context(), &i); err != nil || len(i.PayloadSHA256) != 64 {
		t.Fatalf("seal after a canceled one: %v, digest %q", err, i.PayloadSHA256)
	}
}

func TestValidateStopsAtItsContextDeadline(t *testing.T) {
	i := contextImage(t, contextObjects)
	var calls atomic.Int64
	memo := NewValidationMemo()
	opts := func(ctx context.Context) Validation {
		o := validation(i)
		o.Memo, o.Context = memo, ctx
		o.ValidateObject = func(json.RawMessage) error {
			// A slow owner decode that does not check the context itself.
			calls.Add(1)
			time.Sleep(contextStep)
			return nil
		}
		return o
	}
	ctx, cancel := context.WithTimeout(t.Context(), contextDeadline)
	defer cancel()
	started := time.Now()
	v, err := Validate(i, opts(ctx))
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) || v != nil {
		t.Fatalf("validation past its deadline returned a validated image: %t, error %v after %s; want the deadline", v != nil, err, elapsed)
	}
	if elapsed > contextPrompt || calls.Load() >= contextObjects {
		t.Fatalf("validation stopped after %s and %d of %d objects, want a prompt stop", elapsed, calls.Load(), contextObjects)
	}
	// The refusal is not remembered: a live validation runs every function.
	before := calls.Load()
	if _, err := Validate(i, opts(t.Context())); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load() - before; got != int64(len(i.Payload.Objects)) {
		t.Fatalf("validation after a canceled one checked %d of %d objects", got, len(i.Payload.Objects))
	}
}

func TestCanceledDigestAndCanonicalFormAreNotRemembered(t *testing.T) {
	i := contextImage(t, 10)
	want := i.PayloadSHA256
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := digestContext(ctx, i); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled digest: %v", err)
	}
	if got, err := Digest(i); err != nil || got != want {
		t.Fatalf("digest after a canceled one: %q, %v, want %q", got, err, want)
	}
	raw, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalImage(ctx, raw, 64); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled canonical form: %v", err)
	}
	if _, err := canonicalImage(t.Context(), raw, 64); err != nil {
		t.Fatalf("canonical form after a canceled one: %v", err)
	}
	if _, err := Validate(i, Validation{SchemaVersion: 2, Kinds: i.KindManifest, Context: ctx}); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation with an ended context: %v", err)
	}
}
