package api

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// currentAdoptionProgram keeps the entire selection in one command: no journal
// or annotation exists unless every reviewed candidate and final admission pass.
func (s *Server) currentAdoptionProgram(input []MigrationAuthorityChange) (*MigrationProgram, *MigrationResult, error) {
	if len(input) == 0 || len(input) > 1000 {
		return nil, nil, errInvalid("Select between 1 and 1000 previous imports to track")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > 64<<20 {
		return nil, nil, errInvalid("adoption exceeds bounded plan capacity")
	}
	var reviewed []MigrationAuthorityChange
	if err := json.Unmarshal(raw, &reviewed); err != nil {
		return nil, nil, err
	}
	seen := map[store.Key]bool{}
	for _, c := range reviewed {
		key := store.Key{Kind: "DNSZone", Namespace: c.Namespace, Name: c.Name}
		if seen[key] {
			return nil, nil, errInvalid("duplicate adoption zone %s", key)
		}
		seen[key] = true
		if c.Name == "" || c.Namespace != "" || c.UID == "" || c.ResourceVersion == "" {
			return nil, nil, errInvalid("Review each cluster-scoped zone's UID and resourceVersion before adoption")
		}
	}
	result := &MigrationResult{Items: []ApplyResult{}}
	program := &MigrationProgram{}
	program.steps = append(program.steps, func(ctx context.Context, tx *sqlite.CommandTx) (*MigrationCommandPlan, error) {
		plan := &importers.Plan{Objects: []map[string]any{}, Warnings: []string{"Existing imports were explicitly added to migration history. Their original import time and source URL are unknown."}}
		k, _ := kindByKind("DNSZone")
		objectBytes := 0
		originals := make([]*v1alpha1.DNSZone, 0, len(reviewed))
		// No mutation methods are called during preparation. In particular, a
		// stale last candidate cannot leave a journal or earlier adopted zones.
		for _, c := range reviewed {
			z := &v1alpha1.DNSZone{}
			if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: c.Name}, z); err != nil {
				return nil, storeError(err)
			}
			encoded, err := json.Marshal(z)
			if err != nil {
				return nil, err
			}
			objectBytes += len(encoded)
			if objectBytes > 64<<20 {
				return nil, errInvalid("adoption object inventory exceeds bounded plan capacity")
			}
			if !adoptableMigrationZone(z) || z.DeletionTimestamp != nil {
				return nil, errConflict("Zone is no longer an untracked API import", "Refresh the previous imports")
			}
			if c.UID != string(z.UID) {
				return nil, errConflict("Zone was replaced after review", "Refresh the previous imports")
			}
			if c.ResourceVersion != z.ResourceVersion {
				return nil, errPrecondition(c.ResourceVersion, z.ResourceVersion)
			}
			if c.ZoneName != z.Spec.ZoneName || !slices.Equal(c.Nameservers, z.Spec.Nameservers) || c.Primary != zonePrimary(z) || c.Error != "" {
				return nil, errConflict("Reviewed zone authority does not match the previous import", "Refresh the previous imports")
			}
			body, err := objectMap(k, z)
			if err != nil {
				return nil, err
			}
			plan.Objects = append(plan.Objects, body)
			originals = append(originals, z)
		}
		run, err := s.buildMigrationRun(ctx, plan, "existing")
		if err != nil {
			return nil, err
		}
		writes := make([]preparedCommandWrite, 0, len(reviewed))
		attrs := []rbac.Attributes{migrationSettingsRequirement()}
		for i, body := range plan.Objects {
			meta := body["metadata"].(map[string]any)
			ann, ok := meta["annotations"].(map[string]any)
			if !ok {
				ann = map[string]any{}
				meta["annotations"] = ann
			}
			ann[migrationRunAnnotation] = run.Name
			prepared, requirements, err := s.prepareMigrationDocument(ctx, tx, body, reviewed[i].ResourceVersion, false)
			if err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(prepared.result.obj.(*v1alpha1.DNSZone).Spec, originals[i].Spec) {
				return nil, errConflict("Zone requires schema changes before adoption", "Review and save the zone before tracking it")
			}
			writes = append(writes, prepared)
			attrs = append(attrs, requirements...)
		}
		items := make([]ApplyResult, 0, len(writes))
		return migrationPlanFor(tx, attrs, func(ctx context.Context, tx *sqlite.CommandTx) error {
			for i, prepared := range writes {
				wr, err := prepared.apply(ctx, tx)
				if err != nil {
					return err // A mutation error poisons tx; never journal or continue.
				}
				item := ApplyResult{Kind: "DNSZone", Name: reviewed[i].Name, Result: wr.result, ResourceVersion: wr.obj.GetResourceVersion()}
				items = append(items, item)
				run.Spec.Items[i] = migrationItem(item)
				run.Spec.Zones[i].UID = string(wr.obj.GetUID())
			}
			now := metav1.NewTime(s.opts.Now())
			run.Spec.Status = "completed"
			run.Spec.FinishedAt = &now
			return saveCurrentMigration(ctx, tx, run)
		}, func() { result.RunID = run.Name; result.Items = items }), nil
	})
	return program, result, nil
}

func (s *Server) adoptCurrentMigration(ctx context.Context, input []MigrationAuthorityChange) (*MigrationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	program, result, err := s.currentAdoptionProgram(input)
	if err != nil {
		return nil, err
	}
	next, err := s.admitMigration(ctx, program)
	if err != nil {
		return nil, err
	}
	if err := next(ctx); err != nil {
		return nil, storeError(err)
	}
	return result, nil
}
