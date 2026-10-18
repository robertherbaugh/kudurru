package api

import (
	"context"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type importPreviewInput struct{ Body importers.MigrationRequest }
type importPreviewOutput struct{ Body *importers.Plan }
type importApplyInput struct{ Body importers.Plan }

// MigrationResult reports partial failures explicitly, just like POST /apply.
// Settings are changed only after every object was applied successfully.
type MigrationResult struct {
	RunID         string        `json:"runID"`
	JournalError  string        `json:"journalError,omitempty"`
	Items         []ApplyResult `json:"items" nullable:"false"`
	Settings      *SettingsBody `json:"settings,omitempty"`
	SettingsError string        `json:"settingsError,omitempty"`
}
type importApplyOutput struct{ Body *MigrationResult }

func (s *Server) registerImports() {
	s.registerMigrationRuns()
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
		run, err := s.newMigrationRun(ctx, &in.Body, "")
		if err != nil {
			return nil, err
		}
		result := &MigrationResult{RunID: run.Name, Items: []ApplyResult{}}
		defer s.finishMigrationRun(ctx, run, result)
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		failed := false
		for i, obj := range in.Body.Objects {
			doc := normalizeNumbers(obj).(map[string]any)
			if doc["kind"] == "DNSZone" {
				meta, ok := doc["metadata"].(map[string]any)
				if ok {
					ann, ok := meta["annotations"].(map[string]any)
					if !ok {
						ann = map[string]any{}
						meta["annotations"] = ann
					}
					ann[migrationRunAnnotation] = run.Name
				}
			}
			r := s.applyOne(ctx, doc)
			run.Spec.Items[i] = migrationItem(r)
			if r.Result != ResultError && r.Kind == "DNSZone" {
				for j := range run.Spec.Zones {
					z := &run.Spec.Zones[j]
					if z.Name == r.Name && z.Namespace == r.Namespace {
						var current v1alpha1.DNSZone
						if e := s.opts.Store.Get(ctx, store.Key{Kind: "DNSZone", Namespace: r.Namespace, Name: r.Name}, &current); e == nil && current.Annotations[migrationRunAnnotation] == run.Name {
							z.UID = string(current.UID)
						}
					}
				}
			}
			result.Items = append(result.Items, r)
			if !s.persistMigrationProgress(ctx, run, result) {
				failed = true
				break
			}
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
