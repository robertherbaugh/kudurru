package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
)

type blockingReadFunc func(context.Context, time.Time) (bool, bool, time.Time, error)

func (f blockingReadFunc) BlockingState(ctx context.Context, now time.Time) (bool, bool, time.Time, error) {
	return f(ctx, now)
}

func TestCapturedBlockingGETStateAndOneObservation(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 500000000, time.UTC)
	until := now.Add(1500 * time.Millisecond).In(time.FixedZone("test-offset", 3600))
	for _, tt := range []struct {
		name                string
		configured, enabled bool
		deadline            time.Time
		timer               bool
	}{
		{"enabled", true, true, time.Time{}, false},
		{"countdown", true, false, until, true},
		{"exact-expiry", true, true, now, false},
		{"expired", true, true, now.Add(-time.Second), false},
		{"forever", true, false, blockingstage.Forever, false},
		{"off-with-finite-deadline", false, false, until, false},
		{"off", false, false, time.Time{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clockCalls, readCalls := 0, 0
			h := newHarness(t, func(o *Options) {
				o.Config.Blocking.Enabled = !tt.configured // Competing live config.
				o.Now = func() time.Time { clockCalls++; return now }
				o.BlockingReads = blockingReadFunc(func(_ context.Context, observed time.Time) (bool, bool, time.Time, error) {
					readCalls++
					if !observed.Equal(now) {
						t.Error("read received a different observation time", observed)
					}
					return tt.configured, tt.enabled, tt.deadline, nil
				})
			})
			var state BlockingState
			h.do(http.MethodGet, "/api/v1/dns/blocking", "viewer", nil).want(http.StatusOK).json(&state)
			if state.Configured != tt.configured || state.Blocking != tt.enabled || state.Scope != "cluster" || state.Node != "node-a" || clockCalls != 1 || readCalls != 1 {
				t.Fatal("GET mixed live config, scope or clock", state, clockCalls, readCalls)
			}
			if tt.timer {
				if state.Timer == nil || *state.Timer != 1.5 || state.Until == nil || !state.Until.Equal(until) || state.Until.Location() != time.UTC {
					t.Fatal("countdown did not retain exact finite UTC deadline", state)
				}
			} else if state.Timer != nil || state.Until != nil {
				t.Fatal("GET fabricated a resume timer", state)
			}
		})
	}
}

func TestCapturedBlockingGETFailureAndCancellationNeverFallback(t *testing.T) {
	now := time.Now()
	for _, failure := range []string{"error", "invalid", "zero-clock", "canceled-before", "canceled-during"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			var cancel context.CancelFunc
			h := newHarness(t, func(o *Options) {
				o.Now = func() time.Time {
					if failure == "zero-clock" {
						return time.Time{}
					}
					return now
				}
				o.BlockingReads = blockingReadFunc(func(context.Context, time.Time) (bool, bool, time.Time, error) {
					calls++
					if failure == "canceled-during" {
						cancel()
					}
					if failure == "error" {
						return true, true, time.Time{}, errors.New("original state unavailable")
					}
					return failure != "invalid", true, time.Time{}, nil
				})
			})
			req := h.req(http.MethodGet, "/api/v1/dns/blocking", "viewer", nil)
			ctx, stop := context.WithCancel(req.Context())
			defer stop()
			cancel = stop
			if failure == "canceled-before" {
				cancel()
			}
			h.serve(req.WithContext(ctx)).want(http.StatusServiceUnavailable)
			if (failure == "zero-clock" || failure == "canceled-before") && calls != 0 {
				t.Fatal("unusable request called the captured reader")
			}
		})
	}
	s := &Server{opts: Options{BlockingReads: blockingReadFunc(func(context.Context, time.Time) (bool, bool, time.Time, error) {
		t.Fatal("nil context reached reader")
		return false, false, time.Time{}, nil
	})}}
	if _, err := s.getBlocking(nil, nil); err == nil { //nolint:staticcheck // Deliberate missing request context.
		t.Fatal("missing context accepted")
	}
}

func TestCapturedBlockingGETRequiresCurrentPOSTAndPreservesLegacy(t *testing.T) {
	stage, err := blockingstage.New(blockingstage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		o.Blocking = stage
		o.BlockingReads = blockingReadFunc(func(context.Context, time.Time) (bool, bool, time.Time, error) {
			t.Fatal("POST consulted read capability")
			return false, false, time.Time{}, nil
		})
	})
	var state BlockingState
	h.do(http.MethodGet, "/api/v1/dns/blocking", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false}).want(http.StatusServiceUnavailable)
	if !stage.Enabled() {
		t.Fatal("missing current command changed the legacy toggle")
	}
	h.srv.opts.BlockingReads = nil
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false}).want(http.StatusOK).json(&state)
	if state.Blocking || state.Scope != "node" || stage.Enabled() {
		t.Fatal("read capability changed legacy POST", state)
	}
	h.do(http.MethodGet, "/api/v1/dns/blocking", "viewer", nil).want(http.StatusOK).json(&state)
	if state.Blocking || state.Scope != "node" {
		t.Fatal("nil capability changed legacy GET", state)
	}
}
