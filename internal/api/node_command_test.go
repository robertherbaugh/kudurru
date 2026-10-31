package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestNodeConfigCommandUsesActualReceiptWithoutLegacyInstall(t *testing.T) {
	var reload *config.Reloadable
	var installed *config.Config
	calls := 0
	h := newHarness(t, func(o *Options) {
		reload = config.NewReloadable(o.Config)
		o.Reload = reload
		o.NodeConfigCommand = func(_ context.Context, next *config.Config, changed []string, _ ...string) (NodeConfigReceipt, error) {
			calls++
			if !slices.Equal(changed, []string{"log.level"}) || next.Log.Level != "debug" {
				t.Fatal("wrong requested values", changed, next.Log)
			}
			// A concurrent accepted local command happened after API planning.
			before := *reload.Current()
			before.Log.Level, before.Log.Format = "warn", "text"
			after := before
			after.Log.Level = next.Log.Level
			installed = &after
			reload.Set(installed)
			return NodeConfigReceipt{Before: &before, After: &after}, nil
		}
	})
	var body SettingsBody
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "debug"}}).want(http.StatusOK).json(&body)
	log := body.Settings["log"].(map[string]any)
	if calls != 1 || reload.Current() != installed || log["level"] != "debug" || log["format"] != "text" || body.Persisted == nil || *body.Persisted || body.Scope != "node" || body.Settings["tsigKeys"] != Redacted {
		t.Fatal("response replaced actual local document or fabricated persistence", body)
	}
	seen := false
	for _, event := range h.audit.Events() {
		if event.Action == "node.config" && event.Result == "success" {
			seen = true
			var before, after map[string]any
			if err := json.Unmarshal(event.Before, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(event.After, &after); err != nil {
				t.Fatal(err)
			}
			if before["log.level"] != "warn" || after["log.level"] != "debug" {
				t.Fatal("audit used stale API plan", event.Before, event.After)
			}
		}
	}
	if !seen {
		t.Fatal("missing local command audit")
	}
	h.do(http.MethodGet, "/api/v1/node/config", "viewer", nil).want(http.StatusOK).json(&body)
	if body.Settings["log"].(map[string]any)["format"] != "text" {
		t.Fatal("local desired GET changed semantics")
	}
}

func TestNodeConfigCommandFailuresNeverFallBack(t *testing.T) {
	for _, kind := range []string{"missing-command", "missing-reload", "empty-receipt", "401", "403", "503"} {
		t.Run(kind, func(t *testing.T) {
			var reload *config.Reloadable
			calls := 0
			h := newHarness(t, func(o *Options) {
				reload = config.NewReloadable(o.Config)
				o.Reload = reload
				o.CanonicalSettings = policyReadSettings{cfg: o.Config}
				o.NodeConfigCommand = func(context.Context, *config.Config, []string, ...string) (NodeConfigReceipt, error) {
					calls++
					switch kind {
					case "401":
						return NodeConfigReceipt{}, newError(http.StatusUnauthorized, KeyUnauthenticated, "expired", "")
					case "403":
						return NodeConfigReceipt{}, newError(http.StatusForbidden, KeyForbidden, "denied", "")
					case "503":
						return NodeConfigReceipt{}, errUnavailable("pending")
					default:
						return NodeConfigReceipt{}, nil
					}
				}
				if kind == "missing-command" {
					o.NodeConfigCommand = nil
				}
				if kind == "missing-reload" {
					o.Reload = nil
				}
			})
			original := reload.Current()
			want := http.StatusServiceUnavailable
			if kind == "missing-command" {
				want = http.StatusNotImplemented
			}
			if kind == "401" {
				want = http.StatusUnauthorized
			}
			if kind == "403" {
				want = http.StatusForbidden
			}
			h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "debug"}}).want(want)
			if reload.Current() != original || (kind == "missing-command" || kind == "missing-reload") && calls != 0 {
				t.Fatal("refusal used legacy local mutation", calls)
			}
		})
	}
}

func TestNodeConfigCommandKeepsValidationAndClusterPath(t *testing.T) {
	var reload *config.Reloadable
	calls := 0
	h := newHarness(t, func(o *Options) {
		o.Config.EnvLocked = []string{"log.level"}
		reload = config.NewReloadable(o.Config)
		o.Reload = reload
		o.NodeConfigCommand = func(_ context.Context, next *config.Config, paths []string, reset ...string) (NodeConfigReceipt, error) {
			calls++
			if !slices.Equal(paths, []string{"log.format"}) || !slices.Equal(reset, []string{"log.format"}) || next.Log.Format != "json" {
				t.Fatal("null did not resolve schema default", paths, next.Log)
			}
			before := *reload.Current()
			after := before
			after.Log.Format = next.Log.Format
			reload.Set(&after)
			return NodeConfigReceipt{Before: &before, After: &after, Persisted: true}, nil
		}
	})
	for _, body := range []map[string]any{{"node": map[string]any{"name": "other"}}, {"cache": map[string]any{"maxEntries": 1}}, {"unknown": true}, {"log": map[string]any{"format": "bad"}}, {"log": map[string]any{"format": 42}}} {
		h.do(http.MethodPatch, "/api/v1/node/config", "admin", body).want(http.StatusBadRequest)
	}
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "debug"}}).want(http.StatusConflict)
	h.do(http.MethodPatch, "/api/v1/node/config", "viewer", map[string]any{"log": map[string]any{"format": "text"}}).want(http.StatusForbidden)
	if calls != 0 {
		t.Fatal("invalid input reached capability")
	}
	var body SettingsBody
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"format": nil}}).want(http.StatusOK).json(&body)
	if !slices.Equal(body.RestartRequired, []string{"log.format"}) || body.Persisted == nil || !*body.Persisted {
		t.Fatal("lost durable receipt or restart-only format semantics", body)
	}
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"cache": map[string]any{"maxEntries": 42}}).want(http.StatusOK)
	if calls != 1 || reload.Current().Cache.MaxEntries != 42 {
		t.Fatal("node capability replaced cluster path")
	}
}

func TestNodeConfigDurableCapabilityAndClusterScopeRoute(t *testing.T) {
	calls := 0
	h := newHarness(t, func(o *Options) {
		o.Reload = config.NewReloadable(o.Config)
		o.NodeConfigCommand = func(context.Context, *config.Config, []string, ...string) (NodeConfigReceipt, error) {
			calls++
			return NodeConfigReceipt{}, errUnavailable("fixture not admitted")
		}
	})
	var schema struct {
		Items []SettingKey `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/settings/_schema", "viewer", nil).want(http.StatusOK).json(&schema)
	for _, key := range schema.Items {
		if key.ReadOnly {
			t.Fatal("installed durable capability reported read-only", key.Path)
		}
	}
	for _, patch := range []map[string]any{{"log": map[string]any{"level": "debug"}}, {"log": map[string]any{"level": nil}}, {"cache": map[string]any{"maxEntries": 1}}} {
		h.do(http.MethodPatch, "/api/v1/settings?scope=node", "admin", patch).want(http.StatusBadRequest)
	}
	h.do(http.MethodPatch, "/api/v1/settings?scope=invalid", "admin", map[string]any{"cache": map[string]any{"maxEntries": 1}}).want(http.StatusBadRequest)
	if calls != 0 || h.srv.config().Cache.MaxEntries == 1 {
		t.Fatal("cluster scope query became an unqualified write alias")
	}
}
