package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func TestMigrationPreviewRequiresSettingsWriteAndDoesNotWrite(t *testing.T) {
	calls := 0
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer source-secret" {
			t.Error("missing source credential")
		}
		switch r.URL.Path {
		case "/api/zones/list":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"zones":[]}}`)
		case "/api/settings/get":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"enableBlocking":true,"blockingType":"NxDomain","blockingAnswerTtl":30}}`)
		case "/api/blocked/export":
			_, _ = fmt.Fprint(w, "tracker.example\n")
		case "/api/allowed/export":
			_, _ = fmt.Fprint(w, "needed.tracker.example\n")
		default:
			t.Errorf("unexpected source request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer source.Close()
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	permissions.Roles["settings-writer"] = []string{"settings.write"}
	authz, err := rbac.NewBindingAuthorizer(permissions, append(apitest.StandardBindings(), apitest.Grant("settings-writer", "settings-writer")))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Authorizer = authz })
	body := importers.MigrationRequest{Source: "technitium", URL: source.URL, Credential: "source-secret", Namespace: "dns"}
	for _, user := range []string{"viewer", "operator", "zoneadmin"} {
		h.do(http.MethodPost, "/api/v1/imports/preview", user, body).want(http.StatusForbidden)
	}
	if calls != 0 {
		t.Fatal("unauthorized preview contacted the source")
	}
	// This role intentionally has no object permissions, but settings.write
	// still permits credentialed requests to the loopback source API.
	r := h.do(http.MethodPost, "/api/v1/imports/preview", "settings-writer", body).want(http.StatusOK)
	if calls == 0 {
		t.Fatal("settings.write did not permit loopback source access")
	}
	if strings.Contains(r.Body.String(), "source-secret") {
		t.Fatal("source credential leaked into preview")
	}
	var plan importers.Plan
	r.json(&plan)
	if len(plan.Objects) != 3 {
		t.Fatalf("preview objects: %d", len(plan.Objects))
	}
	var before ObjectList
	h.do(http.MethodGet, "/api/v1/objects/blocklists", "admin", nil).want(http.StatusOK).json(&before)
	if len(before.Items) != 0 {
		t.Fatal("preview wrote destination objects")
	}
	var result MigrationResult
	h.do(http.MethodPost, "/api/v1/imports/apply", "settings-writer", plan).want(http.StatusOK).json(&result)
	if len(result.Items) != len(plan.Objects) {
		t.Fatal("missing per-object permission results")
	}
	for _, item := range result.Items {
		if item.Result != ResultError || item.ErrorKey != KeyForbidden {
			t.Fatalf("settings.write unexpectedly granted object writes: %+v", item)
		}
	}
	h.do(http.MethodPost, "/api/v1/imports/apply", "admin", plan).want(http.StatusOK).json(&result)
	if len(result.Items) != 3 {
		t.Fatalf("apply: %+v", result)
	}
	for _, r := range result.Items {
		if r.Result == ResultError {
			t.Errorf("apply %s: %s", r.Name, r.Error)
		}
	}
	h.do(http.MethodGet, "/api/v1/objects/blocklists/dns/technitium-block", "admin", nil).want(http.StatusOK)
}

func TestMigrationApplyUsesExistingValidationAndSkipsSettingsAfterFailure(t *testing.T) {
	h := newHarness(t)
	plan := importers.Plan{Objects: []map[string]any{{"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList", "metadata": map[string]any{"name": "invalid", "namespace": "default"}, "spec": map[string]any{"sources": []any{}}}}, Settings: map[string]any{"cache": map[string]any{"maxEntries": 1234}}, Warnings: []string{}}
	h.do(http.MethodPost, "/api/v1/imports/apply", "viewer", plan).want(http.StatusForbidden)
	var res MigrationResult
	h.do(http.MethodPost, "/api/v1/imports/apply", "admin", plan).want(http.StatusOK).json(&res)
	if len(res.Items) != 1 || res.Items[0].Result != ResultError || res.Settings != nil {
		t.Fatalf("expected validation error without settings write: %+v", res)
	}
	var settings SettingsBody
	h.do(http.MethodGet, "/api/v1/settings", "admin", nil).want(http.StatusOK).json(&settings)
	if settings.Settings["cache"].(map[string]any)["maxEntries"] == float64(1234) {
		t.Fatal("settings changed after a failed object")
	}
}
