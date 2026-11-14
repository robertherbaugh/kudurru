package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
)

func TestBlockingCommandPOSTDetachedInputAndResponse(t *testing.T) {
	until := time.Date(2026, 10, 15, 12, 0, 10, 0, time.UTC)
	seconds := 3.25
	returned := &BlockingState{Blocking: false, Configured: true, Until: &until, Timer: &seconds, Node: "foreign"}
	calls := 0
	h := newHarness(t, func(o *Options) {
		o.BlockingCommand = func(_ context.Context, change BlockingChange) (*BlockingState, error) {
			calls++
			if change.Blocking || !change.HasTimer || change.TimerSeconds != seconds || !change.HasUntil || !change.Until.Equal(until) {
				t.Fatal("lost explicit input", change)
			}
			return returned, nil
		}
	})
	var got BlockingState
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false, "timer": seconds, "until": until}).want(http.StatusOK).json(&got)
	if calls != 1 || got.Scope != "cluster" || got.Node != "node-a" || got.Blocking || !got.Configured || got.Timer == nil || *got.Timer != seconds || got.Until == nil || !got.Until.Equal(until) {
		t.Fatal("wrong command response", got, calls)
	}
	if returned.Node != "foreign" || returned.Scope != "" {
		t.Fatal("API changed callback result")
	}
	h.do(http.MethodPost, "/api/v1/dns/blocking", "viewer", map[string]any{"blocking": true}).want(http.StatusForbidden)
	if calls != 1 {
		t.Fatal("route denial invoked command")
	}
}

func TestBlockingCommandCanonicalPOSTNeverFallsBack(t *testing.T) {
	stage, err := blockingstage.New(blockingstage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		o.Blocking = stage
		o.BlockingReads = blockingReadFunc(func(context.Context, time.Time) (bool, bool, time.Time, error) {
			t.Fatal("POST consulted GET reader")
			return false, false, time.Time{}, nil
		})
	})
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false}).want(http.StatusServiceUnavailable)
	if !stage.Enabled() {
		t.Fatal("absent canonical capability mutated live toggle")
	}
	h.srv.opts.BlockingReads = nil
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false}).want(http.StatusOK)
	if stage.Enabled() {
		t.Fatal("legacy nil capabilities lost toggle behavior")
	}
}

func TestBlockingCommandPOSTErrorsAndPresence(t *testing.T) {
	calls := 0
	pending := &Error{Status: http.StatusServiceUnavailable, Body: ErrorBody{Key: KeyUnavailable, Message: "committed pending", Hint: map[string]any{"committed": true}}}
	h := newHarness(t, func(o *Options) {
		o.BlockingCommand = func(_ context.Context, change BlockingChange) (*BlockingState, error) {
			calls++
			if change.HasTimer || change.HasUntil || change.TimerSeconds != 0 || !change.Until.IsZero() {
				t.Fatal("fabricated presence", change)
			}
			return nil, pending
		}
	})
	response := h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": true}).want(http.StatusServiceUnavailable)
	if response.m()["error"].(map[string]any)["hint"].(map[string]any)["committed"] != true || calls != 1 {
		t.Fatal("lost committed hint", response.Body)
	}
	h.srv.opts.BlockingCommand = func(context.Context, BlockingChange) (*BlockingState, error) {
		t.Fatal("canceled request invoked command")
		return nil, nil
	}
	req := h.req(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false})
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	h.serve(req.WithContext(ctx)).want(http.StatusServiceUnavailable)
}
