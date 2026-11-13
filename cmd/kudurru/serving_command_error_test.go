package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// TestCurrentCommandStateChangedExplainsItself holds the command path's
// 503 for a writable state that changed while a command was admitted (a
// commit that retired the request's publication, a new writer term, a
// replaced dependency) to the refusal inventory's rule: it says what to do,
// and it carries no Retry-After, because a command whose acknowledgment
// failed after its commit is not to be replayed blindly.
func TestCurrentCommandStateChangedExplainsItself(t *testing.T) {
	for _, cause := range []error{serving.ErrPending, serving.ErrSuperseded, serving.ErrDependency, replication.ErrFenced, primaryruntime.ErrUnavailable} {
		var refusal *api.Error
		if !errors.As(currentCommandAPIError(cause), &refusal) {
			t.Fatalf("%v: not an API error", cause)
		}
		if refusal.Status != http.StatusServiceUnavailable || refusal.Body.Key != api.KeyUnavailable || refusal.Body.Message != api.CommandStateChangedMessage {
			t.Errorf("%v: %d %s %q", cause, refusal.Status, refusal.Body.Key, refusal.Body.Message)
		}
		if hint, _ := refusal.Body.Hint.(string); hint != api.CommandStateChangedHint {
			t.Errorf("%v: hint %q, want %q", cause, hint, api.CommandStateChangedHint)
		}
		if got := refusal.GetHeaders().Get("Retry-After"); got != "" {
			t.Errorf("%v: Retry-After %q", cause, got)
		}
	}
}
