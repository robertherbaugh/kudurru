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

func TestOfflineConvertedSettingsUseCurrentAdmission(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		t.Run(source, func(t *testing.T) {
			s, st, host := migrationSettingsFixture(t)
			host.EnvLocked = []string{"upstreams.groups"}
			input := "dns: {upstream_dns: [192.0.2.53]}"
			preview := importers.PreviewAdGuard
			if source == "blocky" {
				input = "upstreams: {groups: {default: [192.0.2.53]}}"
				preview = importers.PreviewBlocky
			}
			plan, err := preview(t.Context(), importers.OfflineRequest{}, strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			plan.Selection = &importers.Selection{Objects: []string{}, Settings: []string{"upstreams"}}
			before, _ := migrationSettingsRead(t, st, host)
			program, run, result, err := s.currentImportProgram(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < program.Len(); i++ {
				if err := migrationSettingsStep(t.Context(), st, program, i, nil); err != nil {
					t.Fatal(err)
				}
			}
			after, _ := migrationSettingsRead(t, st, host)
			if after.ResourceVersion != before.ResourceVersion || result.Settings != nil || result.SettingsError == "" || (*run).Spec.Status != "partial" || (*run).Spec.Items[len((*run).Spec.Items)-1].ErrorKey != KeyEnvLocked {
				t.Fatal("offline settings bypassed current admission or lost settings report", result, (*run).Spec)
			}
		})
	}
}
