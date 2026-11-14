package replica

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// holdCoordinator returns a follower coordinator whose source counts its
// fetches and blocks each one until fetch is released, when gate is set.
func holdCoordinator(t *testing.T, maxHold time.Duration, gate chan struct{}) (*Coordinator, *atomic.Int32) {
	t.Helper()
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	base := imageOf(t, mirror).Generation()
	fetched := &atomic.Int32{}
	c, err := NewCoordinator(CoordinatorOptions{Store: mirror, NodeName: "b", Interval: 10 * time.Millisecond, MaxPullHold: maxHold,
		Served:            func(context.Context) (replication.Generation, func(), error) { return base, func() {}, nil },
		AuthorizeOperator: func(context.Context) (PromotionAuthorization, error) { return allowedPromotion(), nil },
		Source: sourceFunc(func(ctx context.Context, _ string) (follower.Response, error) {
			fetched.Add(1)
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
				}
			}
			return follower.Response{}, ErrUnavailable
		})})
	if err != nil {
		t.Fatal(err)
	}
	return c, fetched
}

// A forwarded request that holds pulls keeps the scheduled pull from
// running until it releases; an explicit pull does not wait (D375).
func TestCoordinatorScheduledPullWaitsForHolders(t *testing.T) {
	c, fetched := holdCoordinator(t, time.Minute, nil)
	release, err := c.HoldPulls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	if n := fetched.Load(); n != 0 {
		t.Fatalf("the scheduled pull ran %d times while a request held pulls", n)
	}
	_ = c.Pull(t.Context())
	if n := fetched.Load(); n != 1 {
		t.Fatalf("an explicit pull waited for the holder (%d fetches)", n)
	}
	release()
	release() // a second release does nothing
	deadline := time.Now().Add(5 * time.Second)
	for fetched.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the scheduled pull did not run after the hold was released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// The scheduled pull waits at most MaxPullHold for a holder that does not
// release; then it pulls anyway.
func TestCoordinatorScheduledPullHoldIsBounded(t *testing.T) {
	c, fetched := holdCoordinator(t, 200*time.Millisecond, nil)
	release, err := c.HoldPulls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	started := time.Now()
	if err := c.scheduledPull(t.Context()); err == nil {
		t.Fatal("the stub source answered a pull")
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || fetched.Load() != 1 {
		t.Fatalf("scheduled pull after %s with %d fetches, want one after the 200 ms bound", elapsed, fetched.Load())
	}
}

// A hold is granted only after a running pull ended, so that a request that
// captures its source after HoldPulls captures the installed one.
func TestCoordinatorHoldWaitsForARunningPull(t *testing.T) {
	gate := make(chan struct{})
	c, fetched := holdCoordinator(t, time.Minute, gate)
	pulled := make(chan struct{})
	go func() { defer close(pulled); _ = c.Pull(t.Context()) }()
	for fetched.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.HoldPulls(short); err == nil {
		t.Fatal("a hold was granted while a pull was installing")
	}
	granted := make(chan func(), 1)
	go func() {
		release, err := c.HoldPulls(t.Context())
		if err == nil {
			granted <- release
		}
	}()
	select {
	case <-granted:
		t.Fatal("a hold was granted while a pull was installing")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	<-pulled
	select {
	case release := <-granted:
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("the hold was not granted after the pull ended")
	}
}
