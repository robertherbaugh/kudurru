package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestAnchorStartupCancellationAndSuccessor(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	endpoint, _ := anchorWireServer(t, "tcp", reply)
	s, original := directAnchorRuntime(t, endpoint.String(), seed.DS)
	_, oldRelease, err := s.anchorWriterStartup(original)
	if err != nil {
		t.Fatal(err)
	}
	run := func(context.Context) error {
		t.Error("worker ran without a current released startup gate")
		return nil
	}
	if err := s.afterAnchorStartup(t.Context(), run); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("startup admitted work without an elected token", err)
	}
	wait, cancelWait := context.WithCancel(original)
	done := make(chan error, 1)
	go func() { done <- s.afterAnchorStartup(wait, run) }()
	cancelWait()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled startup wait", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup wait did not join cancellation")
	}
	// A real authority transition retires the original elector's token.
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.opts.Store.EnablePrimary(t.Context(), image.Generation()); err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	terms, joined := make(chan context.Context, 1), make(chan error, 1)
	go func() {
		joined <- s.opts.Store.Elector("anchor-startup-successor").Run(life, func(term context.Context) {
			terms <- term
			<-term.Done()
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-joined:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("successor elector did not join")
		}
	})
	var successor context.Context
	select {
	case successor = <-terms:
	case <-time.After(3 * time.Second):
		t.Fatal("successor elector did not start")
	}
	ready, release, err := s.anchorWriterStartup(successor)
	if err != nil {
		t.Fatal(err)
	}
	oldRelease()
	// Remove only cancellation to exercise the original token's authority
	// check, rather than merely observing the canceled original context.
	stale := context.WithoutCancel(original)
	if _, _, err := s.anchorWriterStartup(stale); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("old token replaced the successor gate", err)
	}
	if err := s.afterAnchorStartup(stale, run); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("old worker adopted the successor gate", err)
	}
	select {
	case <-ready:
		t.Fatal("old callback released the successor gate")
	default:
	}
	release()
	started := false
	if err := s.afterAnchorStartup(successor, func(context.Context) error { started = true; return nil }); err != nil || !started {
		t.Fatal("current worker did not start after its own release", err)
	}
}
