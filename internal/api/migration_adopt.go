package api

import (
	"context"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type migrationExistingOutput struct {
	Body *List[MigrationAuthorityChange]
}
type migrationAdoptInput struct {
	Body struct {
		Zones []MigrationAuthorityChange `json:"zones" nullable:"false" minItems:"1" maxItems:"1000"`
	}
}

func (s *Server) registerMigrationAdoption() {
	register(s, operation(http.MethodGet, "/imports/existing", "listExistingImports", "Find previous API imports without a migration run", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, _ *struct{}) (*migrationExistingOutput, error) {
		reader, err := s.migrationReader(ctx)
		if err != nil {
			return nil, err
		}
		list, err := reader.List(ctx, "DNSZone", store.ListOptions{})
		if err != nil {
			return nil, storeError(err)
		}
		items := []MigrationAuthorityChange{}
		for _, obj := range list.Items {
			z := obj.(*v1alpha1.DNSZone)
			if !adoptableMigrationZone(z) {
				continue
			}
			k, _ := kindByKind("DNSZone")
			body, _ := objectMap(k, z)
			if err := s.authorizeWrite(ctx, k, writeRequest{kind: k, body: body}); err != nil {
				continue
			}
			items = append(items, MigrationAuthorityChange{Name: z.Name, Namespace: z.Namespace, ZoneName: z.Spec.ZoneName, ResourceVersion: z.ResourceVersion, Nameservers: append([]string{}, z.Spec.Nameservers...), Primary: zonePrimary(z)})
		}
		return &migrationExistingOutput{Body: newList(items, "")}, nil
	})
	register(s, operation(http.MethodPost, "/imports/adopt", "adoptExistingImports", "Track reviewed previous imports without reimporting records", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *migrationAdoptInput) (*importApplyOutput, error) {
		if s.currentMigrations() {
			return nil, errUnavailable("current migration adoption command unavailable")
		}
		if err := s.readOnly(); err != nil {
			return nil, err
		}
		plan := &importers.Plan{Objects: []map[string]any{}, Warnings: []string{"Existing imports were explicitly added to migration history. Their original import time and source URL are unknown."}}
		k, _ := kindByKind("DNSZone")
		// Validate every candidate before creating the run. Version checks are also
		// enforced at each write, so a competing adoption cannot take ownership.
		for _, candidate := range in.Body.Zones {
			z := &v1alpha1.DNSZone{}
			if err := s.opts.Store.Get(ctx, store.Key{Kind: "DNSZone", Namespace: candidate.Namespace, Name: candidate.Name}, z); err != nil {
				return nil, storeError(err)
			}
			if !adoptableMigrationZone(z) {
				return nil, errConflict("Zone is no longer an untracked API import", "Refresh the previous imports")
			}
			if candidate.ResourceVersion == "" || candidate.ResourceVersion != z.ResourceVersion {
				return nil, errPrecondition(candidate.ResourceVersion, z.ResourceVersion)
			}
			body, err := objectMap(k, z)
			if err != nil {
				return nil, err
			}
			if err := s.authorizeWrite(ctx, k, writeRequest{kind: k, body: body}); err != nil {
				return nil, err
			}
			plan.Objects = append(plan.Objects, body)
		}
		run, err := s.newMigrationRun(ctx, plan, "existing")
		if err != nil {
			return nil, err
		}
		result := &MigrationResult{RunID: run.Name, Items: []ApplyResult{}}
		defer s.finishMigrationRun(ctx, run, result)
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		for i, body := range plan.Objects {
			c := in.Body.Zones[i]
			meta := body["metadata"].(map[string]any)
			ann, ok := meta["annotations"].(map[string]any)
			if !ok {
				ann = map[string]any{}
				meta["annotations"] = ann
			}
			ann[migrationRunAnnotation] = run.Name
			req := writeRequest{kind: k, body: body, ifMatch: c.ResourceVersion}
			item := ApplyResult{Kind: "DNSZone", Namespace: c.Namespace, Name: c.Name, Result: ResultError}
			err := s.authorizeWrite(ctx, k, req)
			if err == nil {
				wr, e := s.writeObject(ctx, req)
				err = e
				if e == nil {
					item.Result = wr.result
					item.ResourceVersion = wr.obj.GetResourceVersion()
					run.Spec.Zones[i].UID = string(wr.obj.GetUID())
				}
			}
			if err != nil {
				item.Error = err.Error()
			}
			result.Items = append(result.Items, item)
			run.Spec.Items[i] = migrationItem(item)
			if !s.persistMigrationProgress(ctx, run, result) {
				break
			}
		}
		return &importApplyOutput{Body: result}, nil
	})
}
func adoptableMigrationZone(z *v1alpha1.DNSZone) bool {
	return z.Spec.Type == v1alpha1.ZoneTypePrimary && store.ManagedBy(z) == store.ManagerAPI && z.Annotations[migrationRunAnnotation] == "" && importers.IsImportedZone(z.Name, z.Spec.ZoneName)
}
