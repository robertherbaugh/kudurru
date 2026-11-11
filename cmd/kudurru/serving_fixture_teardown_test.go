package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// runServingFixtureWriter runs the complete writer under the store's real
// elector until ctx ends. early receives RunWriter's error only when the
// writer ended while its own term context was still alive, which is the
// condition every mounted fixture reports as a writer failure.
func runServingFixtureWriter(ctx context.Context, s *servingRuntime, election string) (early <-chan error, joined <-chan error) {
	ended, done := make(chan error, 1), make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector(election).Run(ctx, func(term context.Context) {
			if err := s.RunWriter(term); err != nil && term.Err() == nil {
				select {
				case ended <- err:
				default:
				}
			}
		}, nil)
	}()
	return ended, done
}

func servingFixtureWriterReady(s *servingRuntime) bool {
	return s.primary.driver.Status().Ready && s.publisher.Status().ProtectedReady
}

// TestServingWriterEndsWhenAlwaysOnRuntimeStopsFirst pins the dependency that
// makes teardown order matter. The elected writer works through the always-on
// runtime (publisher, provider coordinator, host observation). When Run
// returns, the runtime's own lifetime ends with it, and RunWriter returns an
// error although the writer's term context is still alive: context.Canceled
// from the runtime's or the provider coordinator's lifetime, or the first
// error of a component that lost its publication. That is the intended
// contract, because a writer cannot outlive the runtime it writes through, and
// cluster.Runner never produces this order: it cancels the election and joins
// the elected group before it cancels the always-on group. A fixture has to
// keep the same order (servingFixtureAlwaysOn).
func TestServingWriterEndsWhenAlwaysOnRuntimeStopsFirst(t *testing.T) {
	s := newServingRuntimeFixture(t)
	alwaysOn, stopAlwaysOn := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(alwaysOn) }()
	writer, stopWriter := context.WithCancel(context.Background())
	early, joined := runServingFixtureWriter(writer, s, "always-on-first-test")
	runJoined := false
	defer func() {
		stopWriter()
		<-joined
		stopAlwaysOn()
		if !runJoined {
			<-runDone
		}
	}()
	awaitServingRuntime(t, s, func() bool { return servingFixtureWriterReady(s) })

	stopAlwaysOn()
	select {
	case err := <-runDone:
		runJoined = true
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("always-on runtime ended with an unexpected error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("always-on runtime failed to join")
	}
	select {
	case err := <-early:
		// The term context is alive here by construction: only the deferred
		// stopWriter ends it.
		if writer.Err() != nil {
			t.Fatal("fixture canceled the writer before the always-on runtime")
		}
		t.Log("writer ended under a live term after the always-on runtime stopped:", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the writer outlived the always-on runtime")
	}
}

// TestServingFixtureAlwaysOnRuntimeOutlivesTestContext reproduces the teardown
// order behind "browser writer ended context canceled" and proves that the
// mounted fixtures no longer produce it.
//
// The testing package cancels t.Context() before it calls the first cleanup,
// and the cancellation reaches the children of t.Context() in map order. While
// runServingFixture derived the always-on context from t.Context(), that
// context could end before a sibling elector context: the writer then ended
// because the runtime under it had stopped, with its own term context alive
// for a few more microseconds, and the fixture reported a writer failure. The
// held elector context below stands for such a sibling that the cancellation
// has not reached yet. It stays alive until the first cleanup releases it, so
// the short window is a deterministic one here.
func TestServingFixtureAlwaysOnRuntimeOutlivesTestContext(t *testing.T) {
	s := newServingRuntimeFixture(t)
	runServingFixture(t, s)
	held, release := context.WithCancel(context.WithoutCancel(t.Context()))
	early, joined := runServingFixtureWriter(held, s, "fixture-teardown-order-test")
	// Registered last, so it runs first: after the testing package has canceled
	// t.Context() and before runServingFixture stops the always-on runtime.
	t.Cleanup(func() {
		defer func() {
			release()
			select {
			case err := <-joined:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("held writer failed to join")
			}
		}()
		if t.Context().Err() == nil {
			t.Error("t.Context() is alive in a cleanup; this test no longer exercises fixture teardown")
			return
		}
		// Two further publications need a live always-on publisher. A publisher
		// whose context has ended starts no preparation, so at most the one
		// publication that was already in flight can still register, and a
		// closed publisher reports publication zero.
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for range 2 {
			before := s.publisher.Status().Served.Publication
			s.publisher.Wake()
			for s.publisher.Status().Served.Publication <= before {
				select {
				case err := <-early:
					t.Error("the always-on runtime stopped with the test context and ended the writer while its own context was alive:", err)
					return
				case <-deadline.C:
					t.Error("the always-on runtime stopped publishing when the test context ended")
					return
				case <-tick.C:
				}
			}
		}
		if err := s.life.Err(); err != nil {
			t.Error("the always-on runtime ended before its own cleanup:", err)
		}
		select {
		case err := <-early:
			t.Error("the writer ended while its own context was alive:", err)
		default:
		}
	})
	awaitServingRuntime(t, s, func() bool { return servingFixtureWriterReady(s) })
}
