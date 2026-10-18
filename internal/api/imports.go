package api

import (
	"context"
	"net/http"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
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
type migrationStartOutput struct {
	Location string `header:"Location"`
	Body     MigrationRun
}

func (s *Server) registerImports() {
	s.registerMigrationRuns()
	preview := operation(http.MethodPost, "/imports/preview", "previewMigration", "Preview an API migration from Pi-hole v6 or Technitium", "imports")
	preview.Description = "Reads the source using transient credentials; returns objects, a settings merge patch and conversion warnings. Requires settings.write, which permits credentialed requests to caller-selected HTTP(S) targets, including loopback, link-local and internal service addresses. This grants SSRF-equivalent network access even without other administrator permissions; grant it only to trusted administrators and use network egress controls to restrict reachable targets. Source HTTP failure status codes are reported, but error response bodies and credentials are not exposed. Credentials are never stored. No destination writes occur."
	register(s, withStatus(operation(http.MethodPost, "/imports/start", "startMigration", "Start a reviewed migration in the background", "imports"), http.StatusAccepted), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *importApplyInput) (*migrationStartOutput, error) {
		run, err := s.startMigration(ctx, &in.Body)
		if err != nil {
			return nil, err
		}
		return &migrationStartOutput{Body: *run, Location: s.prefix + "/api/v1/imports/runs/" + run.ID}, nil
	})
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
		ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
		defer cancel()
		result := s.executeMigration(ctx, run, &in.Body)
		return &importApplyOutput{Body: result}, nil
	})
}

// executeMigration is shared by the legacy synchronous endpoint and jobs.
func (s *Server) executeMigration(ctx context.Context, run *internal.MigrationRun, plan *importers.Plan) (result *MigrationResult) {
	_, settings, _ := plan.Selected()
	result = &MigrationResult{RunID: run.Name, Items: []ApplyResult{}}
	defer func() {
		if recovered := recover(); recovered != nil {
			s.log.Error("migration worker panicked", "run", run.Name, "panic", recovered)
			run.Spec.Failure = "Import stopped unexpectedly; review the destination to confirm any in-flight writes."
			for i := range run.Spec.Items {
				if run.Spec.Items[i].Result == "running" {
					run.Spec.Items[i].Result = "unconfirmed"
					run.Spec.Items[i].Hint = "Check the destination before retrying this item."
				}
			}
		}
		s.finishMigrationRun(ctx, run, result)
	}()
	failed := false
	nextPrepared := false
	for i, obj := range plan.Objects {
		if run.Spec.Items[i].Result == "skipped" {
			continue
		}
		if ctx.Err() != nil {
			run.Spec.Failure = "Import stopped before all selected items were attempted: " + ctx.Err().Error()
			failed = true
			break
		}
		run.Spec.Items[i].Result = "running"
		if !nextPrepared && !s.persistMigrationProgress(ctx, run, result) {
			failed = true
			break
		}
		doc, _ := normalizeNumbers(obj).(map[string]any)
		if doc["kind"] == "DNSZone" || doc["kind"] == "DNSRecordSet" {
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
		// Save this outcome and the next in-flight marker together. The first
		// object needs its own marker; later objects reuse the preceding save.
		nextPrepared = false
		for j := i + 1; j < len(plan.Objects); j++ {
			if run.Spec.Items[j].Result == "pending" {
				run.Spec.Items[j].Result = "running"
				nextPrepared = true
				break
			}
		}
		if !s.persistMigrationProgress(ctx, run, result) {
			failed = true
			break
		}
		if r.Result == ResultError {
			failed = true
		}
	}
	if ctx.Err() != nil {
		run.Spec.Failure = "Import stopped: " + ctx.Err().Error()
		failed = true
	}
	if !failed && len(settings) > 0 {
		for i := len(plan.Objects); i < len(run.Spec.Items); i++ {
			if run.Spec.Items[i].Result != "skipped" {
				run.Spec.Items[i].Result = "running"
			}
		}
		failed = !s.persistMigrationProgress(ctx, run, result)
	}
	if !failed && len(settings) > 0 {
		out, err := s.patchSettings(ctx, config.ScopeCluster, settings)
		if err != nil {
			result.SettingsError = err.Error()
		} else {
			result.Settings = out.Body
			run.Spec.SettingsPersisted = out.Body.Persisted
		}
	}
	for i := len(plan.Objects); i < len(run.Spec.Items); i++ {
		item := &run.Spec.Items[i]
		if item.Result == "skipped" {
			continue
		}
		switch {
		case failed:
			item.Result = "not_applied"
			item.Hint = "Settings were not changed because a selected object failed or the import stopped."
		case result.SettingsError != "":
			item.Result = ResultError
			item.Error = truncateMigrationError(result.SettingsError)
		default:
			item.Result = ResultUpdated
		}
	}
	return result
}
