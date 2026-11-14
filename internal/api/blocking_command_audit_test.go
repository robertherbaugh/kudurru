package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

func TestBlockingCommandExternalAuditPreservesOutcomes(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHarness(t, func(o *Options) {
			o.AuditBlockingCommand = true
			o.BlockingCommand = func(context.Context, BlockingChange) (*BlockingState, error) {
				if fail {
					return nil, &Error{Status: http.StatusServiceUnavailable, Body: ErrorBody{Key: KeyUnavailable, Message: "unknown completion", Hint: map[string]any{"outcome": "unknown"}}}
				}
				return &BlockingState{Configured: true, Blocking: false}, nil
			}
		})
		want := http.StatusOK
		result := audit.ResultSuccess
		if fail {
			want, result = http.StatusServiceUnavailable, audit.ResultError
		}
		h.do(http.MethodPost, BasePath+"/dns/blocking", "operator", map[string]any{"blocking": false}).want(want)
		events, err := h.audit.Read(t.Context(), audit.Query{})
		if err != nil || len(events) != 1 || events[0].Action != "blocking.toggle" || events[0].Result != result || events[0].Actor.Name != "operator" || events[0].Authz == "" {
			t.Fatal("runtime command audit lost actual outcome or actor", err, events)
		}
	}
}
