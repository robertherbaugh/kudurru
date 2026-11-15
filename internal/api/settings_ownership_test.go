package api

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/settings"
)

func ownershipHarness(t *testing.T) (*harness, *settings.Manager) {
	t.Helper()
	return ownershipHarnessForDriver(t, "kube")
}

func ownershipHarnessForDriver(t *testing.T, driver string) (*harness, *settings.Manager) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "settings.yaml")
	env := []string{config.EnvSettingsFile + "=" + file, "KUDURRU_STORE__DRIVER=" + driver}
	// File persistence makes API authorization/validation tests independent of a cluster.
	h := newHarness(t, func(o *Options) {
		cfg, err := config.Load("", env)
		if err != nil {
			t.Fatal(err)
		}
		o.Config = cfg
		o.Reload = config.NewReloadable(cfg)
		m := &settings.Manager{Backend: settings.FileBackend{Path: file}, Reload: o.Reload, Environ: env}
		if _, err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		o.SettingsOwnership = m
	})
	return h, h.srv.opts.SettingsOwnership
}

func TestSettingsOwnershipAPI(t *testing.T) {
	h, m := ownershipHarness(t)
	var body SettingsBody
	h.do(http.MethodGet, "/api/v1/settings", "viewer", nil).want(http.StatusOK).json(&body)
	revision := body.Sections["upstreams"].Revision
	if revision == "" || body.Sections["upstreams"].Owner != "gitops" {
		t.Fatal("missing ownership")
	}
	path := "/api/v1/settings/sections/upstreams/ownership/adopt"
	h.do(http.MethodPost, path, "viewer", map[string]string{"revision": revision}).want(http.StatusForbidden)
	h.do(http.MethodPost, path, "admin", map[string]string{"revision": "stale"}).want(http.StatusPreconditionFailed)
	before := m.Reload.Current().Upstreams
	h.do(http.MethodPost, path, "admin", map[string]string{"revision": revision}).want(http.StatusOK).json(&body)
	if !reflect.DeepEqual(before, m.Reload.Current().Upstreams) {
		t.Fatal("adopt changed routing")
	}
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"cache": map[string]any{"maxEntries": 10}}).want(http.StatusConflict)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"upstreams": map[string]any{"bootstrap": []string{"9.9.9.9:53"}}}).want(http.StatusOK)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"upstreams": map[string]any{"ecs": map[string]any{"mode": "invalid"}}}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"listeners": map[string]any{"api": map[string]any{"address": ":99"}}}).want(http.StatusBadRequest)
	var yaml settingsYAMLBody
	h.do(http.MethodPost, "/api/v1/settings/sections/upstreams/yaml", "viewer", map[string]any{}).want(http.StatusOK).json(&yaml)
	if yaml.YAML == "" || yaml.Revision == "" {
		t.Fatal("empty export")
	}
	h.do(http.MethodPost, "/api/v1/settings/sections/cache/ownership/adopt", "admin", map[string]string{"revision": revision}).want(http.StatusBadRequest)
	// The mocked file backend holds UI values in the file, so it already matches.
	before = m.Reload.Current().Upstreams
	h.do(http.MethodPost, "/api/v1/settings/sections/upstreams/ownership/release", "admin", map[string]string{"revision": yaml.Revision}).want(http.StatusOK)
	if !reflect.DeepEqual(before, m.Reload.Current().Upstreams) {
		t.Fatal("release changed routing")
	}
	actions := map[string]int{}
	for _, event := range h.audit.Events() {
		if event.Result == "success" {
			actions[event.Action]++
		}
	}
	if actions["settings.adopt"] != 1 || actions["settings.release"] != 1 || actions["settings.write"] != 1 {
		t.Fatalf("audit counts: %v", actions)
	}
}

func TestSettingsOwnershipPatchRevisionAndSourceFailure(t *testing.T) {
	h, m := ownershipHarness(t)
	old := m.Status().Sections["upstreams"].Revision
	if err := m.Transition(t.Context(), "upstreams", "adopt", old); err != nil {
		t.Fatal(err)
	}
	req := h.req(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"upstreams": map[string]any{"bootstrap": []string{"9.9.9.9:53"}}}, "If-Match", old)
	h.serve(req).want(http.StatusPreconditionFailed)
	// Invalid persisted metadata is not silently adopted or served as fresh state.
	if err := os.WriteFile(m.Reload.Current().SettingsFile, []byte("settingsOwnership: {upstreams: invalid}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodGet, "/api/v1/settings", "admin", nil).want(http.StatusServiceUnavailable)
}

func TestSettingsRevisionOnlyGuardsUpstreamEdits(t *testing.T) {
	h, m := ownershipHarnessForDriver(t, "sqlite")
	old := m.Status().Sections["upstreams"].Revision
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"upstreams": map[string]any{"bootstrap": []string{"192.0.2.1:53"}}}).want(http.StatusOK)
	cache := map[string]any{"cache": map[string]any{"maxEntries": 42}}
	h.serve(h.req(http.MethodPatch, "/api/v1/settings", "admin", cache, "If-Match", old)).want(http.StatusOK)
	if m.Reload.Current().Cache.MaxEntries != 42 || m.Reload.Current().Upstreams.Bootstrap[0] != "192.0.2.1:53" {
		t.Fatal("unrelated cache edit lost the current upstream settings")
	}
}

func TestCanonicalSettingsExcludeLegacyOwnership(t *testing.T) {
	h, m := ownershipHarness(t)
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Cache.MaxEntries = 12345
	h.srv.opts.CanonicalSettings = policyReadSettings{cfg: cfg}
	before := m.Status()
	var body SettingsBody
	h.do(http.MethodGet, "/api/v1/settings", "admin", nil).want(http.StatusOK).json(&body)
	if len(body.Sections) != 0 || body.Settings["cache"].(map[string]any)["maxEntries"] != float64(12345) {
		t.Fatal("legacy ownership replaced canonical settings")
	}
	// The refusal names the commands that do work on this runtime.
	for path, command := range map[string]string{
		"/api/v1/settings/sections/upstreams/ownership/adopt":   "kudurru settings set",
		"/api/v1/settings/sections/upstreams/ownership/release": "kudurru settings set",
		"/api/v1/settings/sections/upstreams/yaml":              "kudurru settings get",
	} {
		body := map[string]string{}
		if strings.Contains(path, "/ownership/") {
			body["revision"] = before.Sections["upstreams"].Revision
		}
		r := h.do(http.MethodPost, path, "admin", body).want(http.StatusServiceUnavailable)
		failure, _ := r.m()["error"].(map[string]any)
		hint, _ := failure["hint"].(string)
		if r.errKey() != KeyUnavailable || !strings.Contains(hint, command) {
			t.Fatalf("%s: key %q hint %q", path, r.errKey(), hint)
		}
	}
	if !reflect.DeepEqual(before, m.Status()) {
		t.Fatal("canonical API mutated legacy ownership")
	}
}

// D370 (F28-GATE-1). A settings PATCH on the Kubernetes driver that switches
// the UI-owned upstreams section to anchorMode managed fails validation
// before anything is stored, and the running configuration stays static.
func TestSettingsPatchRefusesManagedRootAnchorsOnKube(t *testing.T) {
	h, m := ownershipHarness(t)
	if err := m.Transition(t.Context(), "upstreams", "adopt", m.Status().Sections["upstreams"].Revision); err != nil {
		t.Fatal(err)
	}
	seed := "12345 15 2 " + strings.Repeat("ab", 32)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"upstreams": map[string]any{"recursion": map[string]any{"dnssec": true, "trustAnchors": []string{seed}}}}).want(http.StatusOK)
	before, status := m.Snapshot()
	file := before.SettingsFile
	raw, err := os.ReadFile(file) //nolint:gosec // G304: the test's temp directory
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"upstreams": map[string]any{"recursion": map[string]any{"anchorMode": "managed"}}}).want(http.StatusBadRequest)
	if !strings.Contains(resp.Body.String(), "require store.driver sqlite") {
		t.Fatal("refusal does not name the driver requirement", resp.Body.String())
	}
	after, afterStatus := m.Snapshot()
	if after != before || after.Upstreams.Recursion.AnchorMode != config.RootAnchorsStatic || !reflect.DeepEqual(afterStatus, status) {
		t.Fatal("refused PATCH changed the running configuration")
	}
	if now, err := os.ReadFile(file); err != nil || string(now) != string(raw) { //nolint:gosec // G304: the test's temp directory
		t.Fatal("refused PATCH wrote the settings source", err)
	}
}
