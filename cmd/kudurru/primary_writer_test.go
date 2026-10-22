package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func TestPrimaryWriterRetainsActualElectorAndRequestLifetimes(t *testing.T) {
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(filepath.Join(t.TempDir(), sqlite.FileName), sqlite.Options{Validator: v})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w := &primaryWriter{store: s}
	if err := w.run(t.Context(), func(context.Context, primaryruntime.PublicationFence) error {
		t.Fatal("missing elector token entered driver")
		return nil
	}); !errors.Is(err, primaryruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	type admitted struct {
		ctx   context.Context
		fence primaryruntime.PublicationFence
	}
	runs := make(chan admitted, 2)
	errs := make(chan error, 2)
	join := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(join) }) }
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- s.Elector("primary-test").Run(ctx, func(term context.Context) {
			errs <- w.run(term, func(ctx context.Context, fence primaryruntime.PublicationFence) error {
				runs <- admitted{ctx, fence}
				<-ctx.Done()
				<-join // Prove the next writer cannot start before joined work.
				return ctx.Err()
			})
		}, nil)
	}()
	t.Cleanup(func() {
		finish()
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("elector failed to join")
		}
	})
	waitRun := func() admitted {
		t.Helper()
		select {
		case a := <-runs:
			return a
		case <-time.After(5 * time.Second):
			t.Fatal("writer did not start")
		}
		return admitted{}
	}
	first := waitRun()
	if err := w.run(first.ctx, func(context.Context, primaryruntime.PublicationFence) error { return nil }); !errors.Is(err, primaryruntime.ErrConflict) {
		t.Fatal("overlapping writer accepted", err)
	}
	type nativeKey struct{}
	native := context.WithValue(t.Context(), nativeKey{}, "native proof marker")
	request, cancel := context.WithTimeout(native, time.Minute)
	bound, release, err := w.bind(request, first.fence)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if bound.Value(nativeKey{}) != native.Value(nativeKey{}) {
		t.Fatal("native proof context was replaced")
	}
	if got, _ := bound.Deadline(); got != deadlineOf(request) {
		t.Fatal("original request deadline lost")
	}
	token, ok := sqlite.WriterTokenFromContext(bound)
	if !ok || token.Term() != first.fence.Term || token.Incarnation() != first.fence.Incarnation {
		t.Fatal("copied writer token lost original incarnation")
	}
	if err := s.Command(bound, first.fence.Term, func(_ context.Context, tx *sqlite.CommandTx) error {
		if tx.Incarnation() != first.fence.Incarnation {
			return errors.New("wrong command incarnation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !errors.Is(bound.Err(), context.Canceled) || first.ctx.Err() != nil {
		t.Fatal("request cancellation did not stay independent")
	}
	held, releaseHeld, err := w.bind(native, first.fence)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHeld()
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnablePrimary(t.Context(), image.Generation()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("elected writer loss did not cancel request")
	}
	if err := s.Command(held, first.fence.Term, func(context.Context, *sqlite.CommandTx) error {
		t.Fatal("old request reached transaction after writer loss")
		return nil
	}); err == nil {
		t.Fatal("old request adopted successor")
	}
	select {
	case <-runs:
		t.Fatal("successor began before old worker joined")
	default:
	}
	finish()
	second := waitRun()
	if second.fence == first.fence || token.Incarnation() != first.fence.Incarnation || token.Term() != first.fence.Term {
		t.Fatal("retained token changed with successor")
	}
	if _, release, err := w.bind(native, first.fence); err == nil || release != nil {
		t.Fatal("old selected serving fence rebound to successor")
	}
	fresh, releaseFresh, err := w.bind(native, second.fence)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFresh()
	if err := s.Command(fresh, second.fence.Term, func(context.Context, *sqlite.CommandTx) error { return nil }); err != nil {
		t.Fatal("fresh elected request cannot execute", err)
	}
	stop()
	select {
	case <-fresh.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel new request")
	}
}

func deadlineOf(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}
