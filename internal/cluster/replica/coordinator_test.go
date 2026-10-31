package replica

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

type sourceFunc func(context.Context, string) (follower.Response, error)

func (f sourceFunc) Fetch(ctx context.Context, etag string) (follower.Response, error) {
	return f(ctx, etag)
}
func TestCoordinatorMonotonicEligibilityServingCASAndRestart(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	base := imageOf(t, mirror).Generation()
	var elapsed time.Duration
	served := base
	deny := false
	options := CoordinatorOptions{Store: mirror, NodeName: "follower-b", Interval: time.Second, Elapsed: func() time.Duration { return elapsed }, Source: sourceFunc(func(context.Context, string) (follower.Response, error) { return follower.Response{}, ErrUnavailable }), Served: func(context.Context) (replication.Generation, func(), error) { return served, func() {}, nil }, AuthorizeOperator: func(context.Context) error {
		if deny {
			return ErrForbidden
		}
		return nil
	}}
	c, e := NewCoordinator(options)
	if e != nil {
		t.Fatal(e)
	}
	req := PromoteRequest{ConfirmNode: "follower-b", ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "operator-promote"}
	if _, e := c.Promote(ctx, req); !errors.Is(e, ErrIneligible) {
		t.Fatalf("restart without missed pulls eligible: %v", e)
	}
	_ = c.Pull(ctx)
	elapsed = time.Second
	_ = c.Pull(ctx)
	if c.Status(ctx).Eligible {
		t.Fatal("promotion before two full intervals")
	}
	elapsed = 2 * time.Second
	_ = c.Pull(ctx)
	if !c.Status(ctx).Eligible {
		t.Fatal("spaced monotonic missed pulls did not grant eligibility")
	}
	restarted, e := NewCoordinator(options)
	if e != nil {
		t.Fatal(e)
	}
	if restarted.Status(ctx).Eligible {
		t.Fatal("restart inherited arbitrary last-contact eligibility")
	}
	served = replication.Generation{}
	if _, e := c.Promote(ctx, req); !errors.Is(e, ErrIneligible) {
		t.Fatalf("unpublished installed generation promoted: %v", e)
	}
	served = base
	bad := req
	bad.ConfirmNode = "wrong"
	if _, e := c.Promote(ctx, bad); !errors.Is(e, ErrIneligible) {
		t.Fatalf("wrong typed node promoted: %v", e)
	}
	bad = req
	bad.ExpectedInstalled.Digest = "wrong"
	if _, e := c.Promote(ctx, bad); !errors.Is(e, replication.ErrCAS) {
		t.Fatalf("stale operator base promoted: %v", e)
	}
	deny = true
	if _, e := c.Promote(ctx, req); !errors.Is(e, ErrForbidden) {
		t.Fatalf("unauthorized operator promoted: %v", e)
	}
	deny = false
	a, e := c.Promote(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	again, e := c.Promote(ctx, req)
	if e != nil || !again.Term.Same(a.Term) {
		t.Fatalf("promotion retry allocated new term: %v", e)
	}
}

func TestCoordinatorReachableRejectedDataNeverGrantsPromotion(t *testing.T) {
	for _, failure := range []error{&ExchangeError{Reached: true, Err: replication.ErrIncompatible}, ErrUnauthorized, ErrUntrusted, context.Canceled} {
		ctx := context.Background()
		p := openStore(t, replication.Primary)
		mirror := mirrorOf(t, p)
		base := imageOf(t, mirror).Generation()
		var elapsed time.Duration
		next := error(ErrUnavailable)
		c, e := NewCoordinator(CoordinatorOptions{Store: mirror, NodeName: "b", Interval: time.Second, Elapsed: func() time.Duration { return elapsed }, Served: func(context.Context) (replication.Generation, func(), error) { return base, func() {}, nil }, AuthorizeOperator: func(context.Context) error { return nil }, Source: sourceFunc(func(context.Context, string) (follower.Response, error) { return follower.Response{}, next })})
		if e != nil {
			t.Fatal(e)
		}
		_ = c.Pull(ctx)
		elapsed = 3 * time.Second
		_ = c.Pull(ctx)
		if !c.Status(ctx).Eligible {
			t.Fatal("fixture not eligible after network loss")
		}
		next = failure
		_ = c.Pull(ctx)
		if c.Status(ctx).Eligible {
			t.Fatalf("reachable/rejected/cancelled primary grants eligibility: %v", failure)
		}
	}
}
