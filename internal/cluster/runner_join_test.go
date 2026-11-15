package cluster

import (
	"context"
	"testing"
	"time"
)

func TestJoinShutdownRetainsBorrowedResourcesUntilWorkerReturns(t *testing.T) {
	r, err := New(Options{JoinShutdown: true, ShutdownTimeout: 10 * time.Millisecond, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if err := r.Add("borrowed", false, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	<-started
	cancel()
	<-canceled
	select {
	case err := <-done:
		close(release)
		t.Fatal("Run returned with borrowed resource still in use", err)
	case <-time.After(4 * r.opts.ShutdownTimeout):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not join released worker")
	}
}
