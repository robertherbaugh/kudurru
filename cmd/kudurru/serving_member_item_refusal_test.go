package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// A child deadline can fire before the original context's timer is delivered.
// The forward validates its absolute deadline independently of those timers.
type memberDeadlineAwaitingTimer struct {
	context.Context
	deadline time.Time
}

func (c memberDeadlineAwaitingTimer) Deadline() (time.Time, bool) { return c.deadline, true }

func TestMemberForwardFailureBeforeOriginalDeadlineTimer(t *testing.T) {
	expired := memberDeadlineAwaitingTimer{Context: t.Context(), deadline: time.Now().Add(-time.Second)}
	if expired.Err() != nil {
		t.Fatal("the original context timer must not have delivered")
	}
	for _, tc := range []struct {
		name string
		err  error
		sent bool
		key  string
		want string
	}{
		{"expired before send", clusterreplica.ErrExpired, false, api.KeyUnavailable, "30-second forwarding lifetime ended before this write was sent; it was not applied"},
		{"source check after deadline", replication.ErrCAS, false, api.KeyUnavailable, "30-second forwarding lifetime ended before this write was sent; it was not applied"},
		{"exchange might have begun", clusterreplica.ErrOutcomeUnknown, false, "completion_unknown", "inspect the object"},
		{"primary already answered", replication.ErrCAS, true, "completion_unknown", "may have been applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refusal *api.Error
			if err := memberForwardFailure(expired, tc.err, tc.sent); !errors.As(err, &refusal) ||
				refusal.Status != http.StatusServiceUnavailable || refusal.Body.Key != tc.key || !strings.Contains(refusal.Body.Message, tc.want) {
				t.Fatalf("forwarding failure: %v", err)
			}
		})
	}
}

// A follower whose own source or membership changed after the primary
// answered cannot say whether the write was applied: completion_unknown, not
// "cluster authority changed", which says that nothing was made. Before the
// primary answered, the same errors still mean that nothing was applied.
func TestObjectForwardErrorAfterTheAnswerIsUnknown(t *testing.T) {
	for _, cause := range []error{replication.ErrCAS, replication.ErrFenced} {
		var after *api.Error
		if !errors.As(objectForwardError(cause, true), &after) || after.Status != http.StatusServiceUnavailable || after.Body.Key != "completion_unknown" ||
			!strings.Contains(after.Body.Message, "may have been applied") {
			t.Fatalf("%v after the primary answered: %+v", cause, after)
		}
		var before *api.Error
		if !errors.As(objectForwardError(cause, false), &before) || before.Body.Key != api.KeyUnavailable || !strings.HasPrefix(before.Body.Message, "cluster authority changed") {
			t.Fatalf("%v before the primary answered: %+v", cause, before)
		}
	}
}

// An item of a multi-item request that a follower cut says what to do, and
// names the primary; other answers are unchanged (D375).
func TestMemberItemRefusalNamesThePrimary(t *testing.T) {
	cut := memberItemRefusal(memberCaptureError(t.Context(), replication.ErrCAS), "node-a")
	var refusal *api.Error
	if !errors.As(cut, &refusal) || refusal.Body.Key != api.KeyUnavailable || !strings.HasPrefix(refusal.Body.Message, "the follower's source changed") ||
		!strings.Contains(refusal.Body.Message, "the enrolled primary node-a") || !strings.Contains(refusal.Body.Message, "was not applied") {
		t.Fatalf("cut item: %+v", refusal)
	}
	unknown := memberItemRefusal(objectForwardError(replication.ErrCAS, true), "node-a")
	if !errors.As(unknown, &refusal) || refusal.Body.Key != "completion_unknown" || !strings.Contains(refusal.Body.Message, "the items after it were not applied; send them to the enrolled primary node-a") {
		t.Fatalf("item at the cut: %+v", refusal)
	}
	// The cut is recognized by memberCaptureError's exact answer, not by a
	// message that merely starts the same way.
	similar := objectForwardUnavailable("the follower's source changed; retry the login")
	if got := memberItemRefusal(similar, "node-a"); got != similar {
		t.Fatalf("a refusal that only starts like the cut changed: %v", got)
	}
	for _, tc := range []struct {
		name    string
		primary string
		want    string
	}{
		{"named primary", "node-a", memberItemLifetimeEnded + "; send this item to the enrolled primary node-a"},
		{"unnamed primary", "", memberItemLifetimeEnded + "; send this item to the enrolled primary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refusal *api.Error
			if got := memberItemRefusal(objectForwardUnavailable(memberItemLifetimeEnded), tc.primary); !errors.As(got, &refusal) ||
				refusal.Status != http.StatusServiceUnavailable || refusal.Body.Key != api.KeyUnavailable || refusal.Body.Message != tc.want {
				t.Fatalf("item after the lifetime: %v", got)
			}
		})
	}
	similarLifetime := objectForwardUnavailable(memberItemLifetimeEnded + "; review current state")
	if got := memberItemRefusal(similarLifetime, "node-a"); got != similarLifetime {
		t.Fatalf("a refusal that only starts like the lifetime refusal changed: %v", got)
	}
	other := objectForwardUnavailable("the enrolled primary is unavailable; the write was not applied")
	if got := memberItemRefusal(other, "node-a"); got != other {
		t.Fatalf("an unrelated refusal changed: %v", got)
	}
	if got := memberItemRefusal(nil, "node-a"); got != nil {
		t.Fatalf("a committed item became an error: %v", got)
	}
}

// A cookie caller's committed password change that the follower could not
// seal into a session names why (D368).
func TestMemberSessionUnservedNamesTheCause(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "this follower could not install the new session in time"},
		{fmt.Errorf("seal: %w", context.Canceled), "this follower could not install the new session in time"},
		{fmt.Errorf("seal: %w", serving.ErrSuperseded), "this follower's cookie settings changed before it could issue the new session"},
		{errors.New("sealer refused"), "this follower could not issue the new session"},
	} {
		if got := memberSessionUnserved(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}
