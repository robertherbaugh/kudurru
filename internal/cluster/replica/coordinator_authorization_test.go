package replica

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCoordinatorRetainsOriginalAuthorizationThroughPromotion(t *testing.T) {
	for _, outcome := range []string{"committed", "revoked", "canceled", "missing-check", "missing-release", "capture-error"} {
		t.Run(outcome, func(t *testing.T) {
			primary := openStore(t, replication.Primary)
			mirror := mirrorOf(t, primary)
			base := imageOf(t, mirror).Generation()
			before := mirror.Authority()
			var elapsed time.Duration
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var original context.Context
			captures, checks, releases := 0, 0, 0
			operatorHeld, servingHeld := false, false
			c, err := NewCoordinator(CoordinatorOptions{
				Store: mirror, NodeName: "follower", Interval: time.Second,
				Elapsed: func() time.Duration { return elapsed },
				Source: sourceFunc(func(context.Context, string) (follower.Response, error) {
					return follower.Response{}, ErrUnavailable
				}),
				Served: func(context.Context) (replication.Generation, func(), error) {
					servingHeld = true
					return base, func() { servingHeld = false }, nil
				},
				AuthorizeOperator: func(bound context.Context) (PromotionAuthorization, error) {
					captures++
					original = bound
					deadline, ok := bound.Deadline()
					want, _ := ctx.Deadline()
					if !ok || !deadline.Equal(want) {
						t.Error("operator capture renewed the earlier deadline")
					}
					operatorHeld = true
					a := PromotionAuthorization{
						Check: func(checkCtx context.Context) error {
							checks++
							if checkCtx != original || !operatorHeld || !servingHeld {
								t.Error("check replaced its original proof or lost a dependency pin")
							}
							if checks == 2 {
								switch outcome {
								case "revoked":
									return ErrForbidden
								case "canceled":
									cancel()
								}
							}
							return nil
						},
						Release: func() {
							releases++
							operatorHeld = false
							if outcome == "committed" && mirror.Authority().Mode != replication.Primary {
								t.Error("operator pin released before actual commit")
							}
						},
					}
					switch outcome {
					case "missing-check":
						a.Check = nil
					case "missing-release":
						a.Release = nil
						operatorHeld = false // no resource was successfully captured
					case "capture-error":
						return a, ErrForbidden
					}
					return a, nil
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
			req := PromoteRequest{ConfirmNode: "follower", ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "captured-operator"}
			_, err = c.Promote(ctx, req)
			wantReleases := 1
			if outcome == "missing-release" {
				wantReleases = 0
			}
			if captures != 1 || releases != wantReleases || operatorHeld || servingHeld {
				t.Fatal("authorization was reacquired or resources were not released exactly once", captures, checks, releases)
			}
			if outcome == "committed" {
				if err != nil || checks != 2 || mirror.Authority().Mode != replication.Primary {
					t.Fatal("authorized promotion failed", err, checks)
				}
				// The exact same intent is still a new authorization operation.
				c.opts.AuthorizeOperator = func(context.Context) (PromotionAuthorization, error) {
					return PromotionAuthorization{Check: func(context.Context) error { return ErrForbidden }, Release: func() { releases++ }}, nil
				}
				if _, err := c.Promote(ctx, req); !errors.Is(err, ErrForbidden) || releases != 2 {
					t.Fatal("idempotent retry bypassed authorization or release", err, releases)
				}
				return
			}
			want := ErrForbidden
			if outcome == "canceled" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(mirror.Authority(), before) || !imageOf(t, mirror).Generation().Same(base) {
				t.Fatal("refused original proof changed authority or installed image", err)
			}
			if (outcome == "revoked" || outcome == "canceled") && checks != 2 {
				t.Fatal("refusal did not reach the final commit check", checks)
			}
		})
	}
}
