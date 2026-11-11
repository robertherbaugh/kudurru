package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// settingsOnly grants the user "editor" settings.read and settings.write
// and nothing else, the way a custom role could; everyone else gets the
// standard bindings.
type settingsOnly struct{ standard rbac.Authorizer }

func (a settingsOnly) Authorize(ctx context.Context, p rbac.Principal, attrs rbac.Attributes) (rbac.Decision, error) {
	if p.Name == "editor" {
		if attrs.Resource == "settings" {
			return rbac.Decision{Allowed: true, Path: "test"}, nil
		}
		return rbac.Decision{Path: "test", Missing: &attrs, Reason: "not granted"}, nil
	}
	return a.standard.Authorize(ctx, p, attrs)
}

// TestSettingsRawDnstapSinkNeedsQuerylogRaw is the admin-only half of the
// raw override (D372): a change of querylog.sinks that leaves a raw dnstap
// sink in place needs querylog.raw on top of settings.write. Without it
// the change is refused and the denial audited; with it the change is
// written and audited as querylog.raw. A sink that is not raw, and removing
// the raw sink again, need only settings.write.
func TestSettingsRawDnstapSinkNeedsQuerylogRaw(t *testing.T) {
	var written [][]string
	h := newHarness(t, func(o *Options) {
		o.Authorizer = settingsOnly{standard: apitest.Authorizer(apitest.StandardBindings()...)}
		o.Settings = SettingsFunc(func(_ context.Context, next *config.Config, changed []string) error {
			written = append(written, config.RawQuerylogSinks(next.Querylog))
			return nil
		})
	})
	sink := func(raw bool) map[string]any {
		return map[string]any{"querylog": map[string]any{"sinks": []any{map[string]any{
			"name": "collector", "type": "dnstap", "dnstap": map[string]any{"target": "unix:/run/dnstap/dnstap.sock", "raw": raw},
		}}}}
	}
	h.do(http.MethodPatch, "/api/v1/settings", "editor", sink(false)).want(http.StatusOK)
	r := h.do(http.MethodPatch, "/api/v1/settings", "editor", sink(true)).want(http.StatusForbidden)
	if r.errKey() != KeyForbidden {
		t.Fatalf("refusal %s", r.Body.String())
	}
	if raws := config.RawQuerylogSinks(h.srv.config().Querylog); len(raws) != 0 || len(written) != 1 {
		t.Fatalf("a refused raw sink was applied: %v, writes %v", raws, written)
	}
	// An operator cannot write settings at all; the administrator can.
	h.do(http.MethodPatch, "/api/v1/settings", "operator", sink(true)).want(http.StatusForbidden)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", sink(true)).want(http.StatusOK)
	if raws := config.RawQuerylogSinks(h.srv.config().Querylog); len(raws) != 1 || raws[0] != "collector" {
		t.Fatalf("raw sinks after the administrator's change: %v", raws)
	}
	// While a raw sink stays, any change of the list needs querylog.raw.
	h.do(http.MethodPatch, "/api/v1/settings", "editor", sink(true)).want(http.StatusForbidden)
	// Turning raw off again needs only settings.write.
	h.do(http.MethodPatch, "/api/v1/settings", "editor", sink(false)).want(http.StatusOK)

	var denied, granted int
	for _, ev := range h.audit.Events() {
		switch {
		case ev.Action == "querylog.raw" && ev.Result == "denied":
			denied++
		case ev.Action == "querylog.raw" && ev.Result == "success":
			granted++
			if ev.Actor.Name != "admin" || len(ev.After) == 0 {
				t.Errorf("raw audit event %+v", ev)
			}
		}
	}
	if denied != 2 || granted != 1 {
		t.Fatalf("querylog.raw audit events: %d denied, %d granted; want 2 and 1", denied, granted)
	}
}
