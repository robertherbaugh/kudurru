package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

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
