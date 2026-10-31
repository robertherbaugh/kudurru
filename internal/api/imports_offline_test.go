package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/importers"
)

func TestOfflineMigrationSourceIdentity(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		h := newHarness(t)
		plan := &importers.Plan{Source: &importers.SourceInfo{Type: source}, Objects: []map[string]any{}, Settings: map[string]any{}, Warnings: []string{}}
		run, err := h.srv.buildMigrationRun(t.Context(), plan, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Validate(); err != nil {
			t.Fatal(err)
		}
		if run.Spec.Source != source || run.Spec.SourceURL != "" {
			t.Fatal("offline identity retained a location")
		}
		plan.Source.URL = "/secret-marker/config.yaml"
		_, err = h.srv.buildMigrationRun(t.Context(), plan, "")
		if err == nil || strings.Contains(err.Error(), "secret-marker") {
			t.Fatal("offline location accepted or leaked")
		}
		raw, _ := json.Marshal(run)
		if strings.Contains(string(raw), "secret-marker") {
			t.Fatal("offline location journaled")
		}
	}
}
