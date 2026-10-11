package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestSettingsRead(t *testing.T) {
	h := newHarness(t)
	var body SettingsBody
	h.do(http.MethodGet, "/api/v1/settings", "viewer", nil).want(http.StatusOK).json(&body)
	if body.Scope != "cluster" || body.Settings["cache"] == nil || body.Settings["upstreams"] == nil {
		t.Fatalf("settings %+v", body)
	}
	if body.Settings["listeners"] != nil || body.Settings["node"] != nil {
		t.Fatal("node-scope keys in cluster settings")
	}
	h.do(http.MethodGet, "/api/v1/node/config", "viewer", nil).want(http.StatusOK).json(&body)
	if body.Scope != "node" || body.Settings["listeners"] == nil {
		t.Fatalf("node config %+v", body)
	}
	// tsigKeys is writeOnly: redacted.
	if body.Settings["tsigKeys"] != Redacted {
		t.Fatalf("tsigKeys not redacted: %v", body.Settings["tsigKeys"])
	}
	var schema struct {
		Items []SettingKey `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/settings/_schema", "viewer", nil).want(http.StatusOK).json(&schema)
	if len(schema.Items) != len(config.Schema) {
		t.Fatalf("%d schema rows, want %d", len(schema.Items), len(config.Schema))
	}
	for _, k := range schema.Items {
		if k.Path == "listeners.api.address" && (!k.HostOnly || k.Scope != "node") {
			t.Fatalf("listeners.api.address %+v", k)
		}
	}
}

func TestSettingsPatch(t *testing.T) {
	var persisted []string
	h := newHarness(t, func(o *Options) {
		o.Settings = SettingsFunc(func(_ context.Context, next *config.Config, changed []string) error {
			persisted = changed
			if next.Cache.MaxEntries != 5000 {
				t.Errorf("writer saw maxEntries %d", next.Cache.MaxEntries)
			}
			return nil
		})
	})
	var body SettingsBody
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{
		"cache":    map[string]any{"maxEntries": 5000, "minTTL": "10s"},
		"blocking": map[string]any{"defaultResponse": "nxdomain"},
	}).want(http.StatusOK).json(&body)
	if len(body.Changed) != 3 || body.Persisted == nil || !*body.Persisted || len(persisted) != 3 {
		t.Fatalf("patch %+v persisted %v", body, persisted)
	}
	cache := body.Settings["cache"].(map[string]any)
	if cache["maxEntries"] != float64(5000) || cache["minTTL"] != "10s" {
		t.Fatalf("cache after patch %v", cache)
	}
	if got := h.srv.config().Blocking.DefaultResponse; got != "nxdomain" {
		t.Fatalf("in-memory config %q", got)
	}
	// Map keys merge (RFC 7386) instead of being replaced.
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{
		"upstreams": map[string]any{"conditional": map[string]any{"corp.example.": "default"}},
	}).want(http.StatusOK)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{
		"upstreams": map[string]any{"conditional": map[string]any{"lab.example.": "default"}},
	}).want(http.StatusOK).json(&body)
	cond := body.Settings["upstreams"].(map[string]any)["conditional"].(map[string]any)
	if len(cond) != 2 {
		t.Fatalf("conditional after merge %v", cond)
	}

	cases := []struct {
		path  string
		patch map[string]any
		code  int
		key   string
	}{
		{"/api/v1/settings", map[string]any{"listeners": map[string]any{"api": map[string]any{"address": ":1"}}}, http.StatusBadRequest, KeyHostOnly},
		{"/api/v1/settings", map[string]any{"nope": 1}, http.StatusBadRequest, KeyInvalid},
		{"/api/v1/settings", map[string]any{"log": map[string]any{"level": "debug"}}, http.StatusBadRequest, KeyInvalid},
		{"/api/v1/settings", map[string]any{"querylog": map[string]any{"privacy": "everything"}}, http.StatusBadRequest, KeyInvalid},
		{"/api/v1/node/config", map[string]any{"node": map[string]any{"dataDir": "/tmp/x"}}, http.StatusBadRequest, KeyHostOnly},
		{"/api/v1/node/config", map[string]any{"cache": map[string]any{"maxEntries": 1}}, http.StatusBadRequest, KeyInvalid},
	}
	for _, tc := range cases {
		r := h.do(http.MethodPatch, tc.path, "admin", tc.patch)
		if r.Code != tc.code || r.errKey() != tc.key {
			t.Errorf("PATCH %s %v: %d %s, want %d %s (%s)", tc.path, tc.patch, r.Code, r.errKey(), tc.code, tc.key, r.Body.String())
		}
	}
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "debug"}}).want(http.StatusOK).json(&body)
	if body.Settings["log"].(map[string]any)["level"] != "debug" {
		t.Fatalf("node config after patch %v", body.Settings["log"])
	}
	// Changes are audited with before/after.
	var seen bool
	for _, ev := range h.audit.Events() {
		if ev.Action == "settings.write" && ev.Result == "success" && len(ev.Before) > 0 {
			seen = true
		}
	}
	if !seen {
		t.Fatal("settings change not audited")
	}
}

// TestSettingsSchemaMetadata: GET /settings/_schema serves the typed
// metadata (C1) and PATCH rejects what it excludes, naming the key, the
// allowed values and the field path.
func TestSettingsSchemaMetadata(t *testing.T) {
	h := newHarness(t)
	var schema struct {
		Items []SettingKey `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/settings/_schema", "viewer", nil).want(http.StatusOK).json(&schema)
	byPath := map[string]SettingKey{}
	for _, k := range schema.Items {
		if k.Label == "" || k.Group == "" {
			t.Errorf("%s: label %q group %q", k.Path, k.Label, k.Group)
		}
		byPath[k.Path] = k
	}
	dr := byPath["blocking.defaultResponse"]
	var values []string
	for _, o := range dr.Enum {
		values = append(values, o.Value)
	}
	if dr.Label != "Default block response" || dr.Group != "Blocking" || strings.Join(values, ",") != "zeroIP,nxdomain,customIP,refused,blockPage" {
		t.Fatalf("blocking.defaultResponse %+v", dr)
	}
	if p := byPath["upstreams.ecs.ipv4Prefix"]; p.Min == nil || *p.Min != 0 || p.Max == nil || *p.Max != 32 {
		t.Fatalf("ipv4Prefix %+v", p)
	}
	g := byPath["upstreams.groups"].Values
	if g == nil || g.Type != "object" || len(g.Fields) != 3 {
		t.Fatalf("upstreams.groups values %+v", g)
	}
	servers, strategy := g.Fields[0], g.Fields[1]
	if servers.Name != "servers" || !servers.Required || servers.Items == nil || servers.Items.Format != "upstream" {
		t.Fatalf("servers %+v", servers)
	}
	if strategy.Name != "strategy" || len(strategy.Enum) != 4 || strategy.Enum[0].Help == "" || strategy.Default != "parallel_best" {
		t.Fatalf("strategy %+v", strategy)
	}
	if c := byPath["upstreams.conditional"].Values; c == nil || c.Ref != "upstreamGroup" {
		t.Fatalf("conditional values %+v", c)
	}

	for _, tc := range []struct {
		patch       map[string]any
		field, want string
	}{
		{map[string]any{"querylog": map[string]any{"privacy": "everything"}}, "querylog.privacy", "full, anonymize_client, hide_domains, hide_domains_clients, nothing"},
		{map[string]any{"upstreams": map[string]any{"ecs": map[string]any{"ipv4Prefix": 33}}}, "upstreams.ecs.ipv4Prefix", "outside 0-32"},
		{map[string]any{"upstreams": map[string]any{"groups": map[string]any{"lan": map[string]any{"servers": []string{"10.0.0.1"}, "strategy": "fastest"}}}}, "upstreams.groups[lan].strategy", "parallel_best, strict, random, rtt_weighted"},
		{map[string]any{"blocking": map[string]any{"customIP": map[string]any{"v4": "::1"}}}, "blocking.customIP.v4", "not an IPv4 address"},
		{map[string]any{"ratelimit": map[string]any{"slip": 101}}, "ratelimit.slip", "outside 0-100"},
	} {
		r := h.do(http.MethodPatch, "/api/v1/settings", "admin", tc.patch).want(http.StatusBadRequest)
		var env struct {
			Error ErrorBody `json:"error"`
		}
		r.json(&env)
		if env.Error.Key != KeyInvalid || !strings.Contains(env.Error.Message, tc.field) || !strings.Contains(env.Error.Message, tc.want) ||
			len(env.Error.Fields) != 1 || env.Error.Fields[0] != tc.field {
			t.Errorf("PATCH %v: %+v", tc.patch, env.Error)
		}
		if strings.HasPrefix(tc.want, "full") && env.Error.Hint != "querylog.privacy must be one of: "+tc.want {
			t.Errorf("hint %v", env.Error.Hint)
		}
	}
}

func TestSettingsEnvLocked(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.EnvLocked = []string{"cache.maxEntries"} })
	r := h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"cache": map[string]any{"maxEntries": 10}}).want(http.StatusConflict)
	if r.errKey() != KeyEnvLocked {
		t.Fatalf("key %q", r.errKey())
	}
}

func TestFlatSettingsCoversSchema(t *testing.T) {
	cfg := testConfig(t)
	flat := flatSettings(cfg)
	raw := map[string]any{}
	for _, k := range config.Schema {
		if _, ok := flat[k.Path]; !ok {
			t.Errorf("%s missing", k.Path)
		}
		raw[k.Path] = flat[k.Path]
	}
	// Round trip: decoding the flat map gives the same configuration.
	back, err := decodeSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	back2 := flatSettings(back)
	for _, k := range config.Schema {
		if a, b := toJSON(flat[k.Path]), toJSON(back2[k.Path]); a != b {
			t.Errorf("%s: %s != %s", k.Path, a, b)
		}
	}
}
