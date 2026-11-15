package replica

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestPromotionScopeRetainsPinsAndInnerRefusals(t *testing.T) {
	for _, behavior := range []string{"allowed", "missing-apply", "nil-check", "swallowed-refusal", "final-refusal"} {
		t.Run(behavior, func(t *testing.T) {
			p := openStore(t, replication.Primary)
			mirror := mirrorOf(t, p)
			base := imageOf(t, mirror).Generation()
			elapsed := time.Duration(0)
			pinned := false
			checks := 0
			refusal := errors.New("original proof refused")
			var escaped func(func(context.Context) error, sqlite.AuditEntry) error
			c, err := NewCoordinator(CoordinatorOptions{Store: mirror, NodeName: "receiver", Interval: time.Second, Elapsed: func() time.Duration { return elapsed },
				Source: sourceFunc(func(context.Context, string) (follower.Response, error) { return follower.Response{}, ErrUnavailable }),
				Served: func(context.Context) (replication.Generation, func(), error) {
					pinned = true
					return base, func() { pinned = false }, nil
				},
				WithOperator: func(_ context.Context, _ PromoteRequest, apply func(func(context.Context) error, sqlite.AuditEntry) error) error {
					if !pinned {
						t.Error("lower dependencies entered before publication pin")
					}
					escaped = apply
					if behavior == "missing-apply" {
						return nil
					}
					var check func(context.Context) error
					if behavior != "nil-check" {
						check = func(context.Context) error {
							checks++
							if !pinned {
								t.Error("publication pin released before commit")
							}
							if behavior == "swallowed-refusal" || behavior == "final-refusal" && checks == 3 {
								return refusal
							}
							return nil
						}
					}
					err := apply(check, sqlite.AuditEntry{Actor: "User:operator", Authz: "admin", Reason: "reviewed"})
					if behavior == "swallowed-refusal" {
						return nil
					}
					return err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = c.Pull(t.Context())
			elapsed = 3 * time.Second
			_ = c.Pull(t.Context())
			req := PromoteRequest{ConfirmNode: "receiver", ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "operator"}
			result, err := c.Promote(t.Context(), req)
			if behavior == "allowed" {
				if err != nil || result.Mode != replication.Primary {
					t.Fatal("valid scope failed", err)
				}
			} else {
				if err == nil || mirror.Authority().Mode != replication.Follower {
					t.Fatal("refusal swallowed", err)
				}
				if (behavior == "swallowed-refusal" || behavior == "final-refusal") && !errors.Is(err, refusal) {
					t.Fatal("inner refusal lost", err)
				}
			}
			if pinned {
				t.Fatal("publication pin leaked")
			}
			if escaped == nil || !errors.Is(escaped(func(context.Context) error { return nil }, sqlite.AuditEntry{}), ErrForbidden) {
				t.Fatal("escaped scope reused")
			}
		})
	}
}
