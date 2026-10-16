package api

import (
	"context"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

type importPreviewInput struct{ Body importers.MigrationRequest }
type importPreviewOutput struct{ Body *importers.Plan }
type importApplyInput struct{ Body importers.Plan }

// MigrationResult reports partial failures explicitly, just like POST /apply.
// Settings are changed only after every object was applied successfully.
type MigrationResult struct {
	Items         []ApplyResult `json:"items" nullable:"false"`
	Settings      *SettingsBody `json:"settings,omitempty"`
	SettingsError string        `json:"settingsError,omitempty"`
}
type importApplyOutput struct{ Body *MigrationResult }

func (s *Server) registerImports() {
	preview := operation(http.MethodPost, "/imports/preview", "previewMigration", "Preview an API migration from Pi-hole v6 or Technitium", "imports")
	preview.Description = "Reads the source using transient credentials; returns objects, a settings merge patch and conversion warnings. Requires settings.write, which permits credentialed requests to caller-selected HTTP(S) targets, including loopback, link-local and internal service addresses. This grants SSRF-equivalent network access even without other administrator permissions; grant it only to trusted administrators and use network egress controls to restrict reachable targets. Source HTTP failure status codes are reported, but error response bodies and credentials are not exposed. Credentials are never stored. No destination writes occur."
	register(s, preview, perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *importPreviewInput) (*importPreviewOutput, error) {
		if in.Body.Namespace == "" {
			in.Body.Namespace = s.opts.DefaultNamespace
		}
		plan, err := importers.Preview(ctx, in.Body)
		if err != nil {
			return nil, errInvalid("migration: %s", err)
		}
		return &importPreviewOutput{Body: plan}, nil
	})
	apply := operation(http.MethodPost, "/imports/apply", "applyMigration", "Apply a reviewed migration preview", "imports")
	apply.Description = "Uses the normal object validation, ownership, license and per-object permission checks. Writes are not transactional; inspect every result. Settings are applied only after all objects succeed."
	register(s, apply, perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *importApplyInput) (*importApplyOutput, error) {
		if err := s.readOnly(); err != nil {
			return nil, err
		}
		result := &MigrationResult{Items: []ApplyResult{}}
		failed := false
		for _, obj := range in.Body.Objects {
			r := s.applyOne(ctx, normalizeNumbers(obj).(map[string]any))
			result.Items = append(result.Items, r)
			if r.Result == ResultError {
				failed = true
			}
		}
		if !failed && len(in.Body.Settings) > 0 {
			out, err := s.patchSettings(ctx, config.ScopeCluster, in.Body.Settings)
			if err != nil {
				result.SettingsError = err.Error()
			} else {
				result.Settings = out.Body
			}
		}
		return &importApplyOutput{Body: result}, nil
	})
}
