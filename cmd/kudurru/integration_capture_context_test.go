package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
)

// The decode test gives a capture 200 ms of a three-second slow decode. The
// prompt bound leaves room for a slow runner and the race detector while
// staying far below the whole decode.
const (
	captureContextWhole    = 3 * time.Second
	captureContextDeadline = 200 * time.Millisecond
	captureContextPrompt   = captureContextWhole / 2
)

// captureContextFixture is a running SQLite integration runtime with one
// Integration and a published cut, whose host does not capture: only the test
// calls capture, so a test seam it sets is not raced by the host's refresh.
func captureContextFixture(t *testing.T) (*integrationRuntime, *integrationCut) {
	t.Helper()
	s, path := integrationFixture(t, nil, nil)
	r := s.integrations
	options, err := r.hostOptions()
	if err != nil {
		t.Fatal(err)
	}
	options.Capture = func(context.Context) (ih.Input, error) { return ih.Input{}, ih.ErrUnavailable }
	if r.host, err = ih.New(options); err != nil {
		t.Fatal(err)
	}
	integrationApply(t, s, integrationDefinition("captured", path))
	integrationRun(t, s)
	awaitServingRuntime(t, s, func() bool {
		_, err := r.capture(t.Context())
		return err == nil
	})
	r.mu.RLock()
	published := r.cut
	r.mu.RUnlock()
	if published == nil {
		t.Fatal("a successful capture published no cut")
	}
	return r, published
}

func expectCaptureStoppedAtDeadline(t *testing.T, r *integrationRuntime, published *integrationCut, in ih.Input, err error, deadline, prompt, elapsed time.Duration) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) || in.Complete {
		t.Fatalf("capture past its deadline returned complete=%t, %v after %s; want the deadline", in.Complete, err, elapsed)
	}
	if elapsed > prompt {
		t.Fatalf("capture past its %s deadline returned after %s, want a prompt stop", deadline, elapsed)
	}
	r.mu.RLock()
	current := r.cut
	r.mu.RUnlock()
	if current != published {
		t.Fatal("a capture past its deadline published its cut")
	}
}

// A capture whose decode is slow stops at its deadline (#281): it does not
// decode the rest and answer late, and it publishes nothing.
func TestIntegrationCaptureStopsAtItsDeadlineAndPublishesNothing(t *testing.T) {
	r, published := captureContextFixture(t)
	objects := len(published.image.Payload.Objects)
	step := captureContextWhole / time.Duration(objects)
	var steps atomic.Int64
	r.decodeStep = func() {
		steps.Add(1)
		time.Sleep(step)
	}
	defer func() { r.decodeStep = nil }()
	ctx, cancel := context.WithTimeout(t.Context(), captureContextDeadline)
	defer cancel()
	started := time.Now()
	in, err := r.capture(ctx)
	expectCaptureStoppedAtDeadline(t, r, published, in, err, captureContextDeadline, captureContextPrompt, time.Since(started))
	if steps.Load() >= int64(objects) {
		t.Fatalf("capture decoded %d of %d objects after its deadline", steps.Load(), objects)
	}
}

// A capture waits to publish while a command holds the current cut, but no
// longer than its deadline (#281). It then leaves the queue: it publishes
// nothing, and a command that arrives after it is not held back. The capture
// has one second, far more than its own work needs, and the test checks that
// it reached the wait; the command holds the cut for six.
func TestIntegrationCapturePublicationWaitEndsAtItsDeadline(t *testing.T) {
	const deadline, hold = time.Second, 6 * time.Second
	r, published := captureContextFixture(t)
	r.mu.RLock()
	released := make(chan struct{})
	held := time.AfterFunc(hold, func() {
		r.mu.RUnlock()
		close(released)
	})
	t.Cleanup(func() {
		if held.Stop() {
			r.mu.RUnlock()
		} else {
			<-released
		}
	})
	var waited atomic.Bool
	r.beforePublish = func() { waited.Store(true) }
	defer func() { r.beforePublish = nil }()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	started := time.Now()
	in, err := r.capture(ctx)
	elapsed := time.Since(started)
	if !waited.Load() {
		t.Fatalf("capture returned %v after %s before it waited to publish", err, elapsed)
	}
	// Before anything else takes the lock shared: a pending exclusive waiter
	// would hold this acquisition back until the command releases the cut.
	next := make(chan *integrationCut, 1)
	go func() {
		r.mu.RLock()
		cut := r.cut
		r.mu.RUnlock()
		next <- cut
	}()
	select {
	case cut := <-next:
		if cut != published {
			t.Fatal("a later command saw a cut the canceled capture published")
		}
	case <-released:
		t.Fatal("a later command waited for the canceled capture")
	}
	expectCaptureStoppedAtDeadline(t, r, published, in, err, deadline, deadline+1500*time.Millisecond, elapsed)
}
