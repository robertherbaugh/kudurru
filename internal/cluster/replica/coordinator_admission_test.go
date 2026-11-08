package replica

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCoordinatorAdmissionBoundsQueuedRequests(t *testing.T) {
	primary := openStore(t, replication.Primary)
	mirror := mirrorOf(t, primary)
	base := imageOf(t, mirror).Generation()
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var fetches, pins, authorizations atomic.Int32
	c, err := NewCoordinator(CoordinatorOptions{
		Store: mirror, NodeName: "follower", Timeout: 5 * time.Second,
		Source: sourceFunc(func(ctx context.Context, _ string) (follower.Response, error) {
			if fetches.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-resume:
				return follower.Response{}, ErrUnavailable
			case <-ctx.Done():
				return follower.Response{}, ctx.Err()
			}
		}),
		Served: func(context.Context) (replication.Generation, func(), error) {
			pins.Add(1)
			return base, func() {}, nil
		},
		AuthorizeOperator: func(context.Context) error { authorizations.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	holder, joined := make(chan error, 1), make(chan struct{})
	go func() { defer close(joined); holder <- c.Pull(t.Context()) }()
	t.Cleanup(func() {
		once.Do(func() { close(resume) })
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("held pull did not join")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first pull never reached its source")
	}
	for _, operation := range []string{"pull", "status", "promote"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "pull":
					done <- c.Pull(ctx)
				case "status":
					s := c.Status(ctx)
					if s.Eligible || s.Reason != "request_unavailable" {
						done <- errors.New("queued status manufactured eligibility")
					} else {
						done <- ctx.Err()
					}
				case "promote":
					_, err := c.Promote(ctx, PromoteRequest{ConfirmNode: "follower", ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "expired-waiter"})
					done <- err
				}
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("queued request did not retain its original deadline", err)
				}
			case <-time.After(time.Second):
				// Release and join even on regression so a failed test leaves no work.
				once.Do(func() { close(resume) })
				err := <-done
				t.Fatalf("expired request remained blocked behind the active pull: %v", err)
			}
		})
	}
	if fetches.Load() != 1 || pins.Load() != 0 || authorizations.Load() != 0 {
		t.Fatal("expired waiter dispatched dependency work", fetches.Load(), pins.Load(), authorizations.Load())
	}
	once.Do(func() { close(resume) })
	if err := <-holder; !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if got := c.Status(t.Context()); got.MissedPulls != 1 || got.Eligible || pins.Load() != 1 {
		t.Fatal("waiters changed reachability evidence or failed to release admission", got)
	}
}

func TestCoordinatorAdmissionPreservesEarlierDeadline(t *testing.T) {
	primary := openStore(t, replication.Primary)
	mirror := mirrorOf(t, primary)
	base := imageOf(t, mirror).Generation()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	original, _ := ctx.Deadline()
	check := func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(original) {
			t.Error("coordinator replaced the earlier original deadline")
		}
	}
	var fetched, pinned, authorized int
	c, err := NewCoordinator(CoordinatorOptions{
		Store: mirror, NodeName: "follower", Timeout: 10 * time.Second,
		Source: sourceFunc(func(ctx context.Context, _ string) (follower.Response, error) {
			check(ctx)
			fetched++
			return follower.Response{}, ErrUnavailable
		}),
		Served: func(ctx context.Context) (replication.Generation, func(), error) {
			check(ctx)
			pinned++
			return base, func() {}, nil
		},
		AuthorizeOperator: func(ctx context.Context) error { check(ctx); authorized++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pull(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if c.Status(ctx).Eligible {
		t.Fatal("one failed pull granted eligibility")
	}
	_, err = c.Promote(ctx, PromoteRequest{ConfirmNode: "follower", ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "early"})
	if !errors.Is(err, ErrIneligible) || fetched != 1 || pinned != 2 || authorized != 1 {
		t.Fatal("original dependencies were not exercised", err, fetched, pinned, authorized)
	}
}

func TestCoordinatorLateAuthorizationCannotPromote(t *testing.T) {
	primary := openStore(t, replication.Primary)
	mirror := mirrorOf(t, primary)
	base := imageOf(t, mirror).Generation()
	before := mirror.Authority()
	var elapsed time.Duration
	pins := 0
	c, err := NewCoordinator(CoordinatorOptions{
		Store: mirror, NodeName: "follower", Timeout: time.Second, Interval: time.Second,
		Elapsed: func() time.Duration { return elapsed },
		Source:  sourceFunc(func(context.Context, string) (follower.Response, error) { return follower.Response{}, ErrUnavailable }),
		Served:  func(context.Context) (replication.Generation, func(), error) { pins++; return base, func() {}, nil },
		AuthorizeOperator: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
				return errors.New("authorization had no original bound")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pull(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	elapsed = 3 * time.Second
	if err := c.Pull(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if !c.Status(t.Context()).Eligible {
		t.Fatal("fixture is not eligible after spaced network failures")
	}
	pins = 0
	_, err = c.Promote(t.Context(), PromoteRequest{ConfirmNode: "follower", ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "late-authorization"})
	after := mirror.Authority()
	if !errors.Is(err, context.DeadlineExceeded) || pins != 0 || !after.Term.Same(before.Term) || after.Mode != before.Mode || after.Incarnation != before.Incarnation {
		t.Fatal("late authorization reached promotion dependencies or changed authority", err, pins, after)
	}
}
