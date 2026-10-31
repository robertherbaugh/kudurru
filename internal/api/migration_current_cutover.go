package api

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *Server) applyCurrentCutover(ctx context.Context, id string, input *MigrationCutoverPlan) (*MigrationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<20 || len(input.Zones) > 1000 {
		return nil, errInvalid("cutover exceeds plan bounds")
	}
	var reviewed MigrationCutoverPlan
	if err := json.Unmarshal(raw, &reviewed); err != nil {
		return nil, err
	}
	var run *internal.MigrationRun
	var fresh *MigrationCutoverPlan
	result := &MigrationResult{RunID: id, Items: []ApplyResult{}}
	program := &MigrationProgram{}
	program.steps = append(program.steps, func(ctx context.Context, tx *sqlite.CommandTx) (*MigrationCommandPlan, error) {
		r := &internal.MigrationRun{}
		if err := tx.Get(ctx, store.Key{Kind: internal.KindMigrationRun, Name: id}, r); err != nil {
			return nil, storeError(err)
		}
		if r.ResourceVersion != reviewed.Revision {
			return nil, errPrecondition(reviewed.Revision, r.ResourceVersion)
		}
		p, err := s.previewMigrationCutoverFrom(ctx, tx, r, reviewed.Nameservers, reviewed.Addresses, false)
		if err != nil {
			return nil, err
		}
		if len(p.Zones) != len(reviewed.Zones) {
			return nil, errInvalid("Preview every zone in the run before applying")
		}
		for i, z := range p.Zones {
			c := reviewed.Zones[i]
			if c.Name != z.Name || c.Namespace != z.Namespace || c.ResourceVersion != z.ResourceVersion || c.Error != z.Error || !reflect.DeepEqual(c.AddressRecordSet, z.AddressRecordSet) {
				return nil, errConflict("A zone changed after the preview", "Preview the cutover again")
			}
		}
		if len(r.Spec.Cutovers) >= 100 {
			return nil, errInvalid("This migration already has 100 cutover attempts")
		}
		now := metav1.NewTime(s.opts.Now())
		if r.Spec.Status == "running" {
			r.Spec.Status = "partial"
			r.Spec.FinishedAt = &now
		}
		if n := len(r.Spec.Cutovers); n > 0 && r.Spec.Cutovers[n-1].Status == "running" {
			r.Spec.Cutovers[n-1].Status = "partial"
			r.Spec.Cutovers[n-1].FinishedAt = &now
		}
		r.Spec.Cutovers = append(r.Spec.Cutovers, internal.MigrationCutover{StartedAt: now, Actor: s.actor(ctx), Status: "running", Nameservers: p.Nameservers, Addresses: p.Addresses, Items: []internal.MigrationItem{}})
		if err := migrationCutoverCapacity(r, p); err != nil {
			return nil, err
		}
		return migrationPlanFor(tx, []rbac.Attributes{migrationSettingsRequirement()}, func(ctx context.Context, tx *sqlite.CommandTx) error { return saveCurrentMigration(ctx, tx, r) }, func() { run = r; fresh = p }), nil
	})
	// The first step loads the trusted run. The reviewed zone count is checked
	// against that run inside the original current transaction before any writes.
	for i := range reviewed.Zones {
		program.steps = append(program.steps, func(ctx context.Context, tx *sqlite.CommandTx) (*MigrationCommandPlan, error) {
			r, err := currentMigration(ctx, tx, run)
			if err != nil {
				return nil, err
			}
			c := fresh.Zones[i]
			item := ApplyResult{Kind: "DNSZone", Namespace: c.Namespace, Name: c.Name}
			attrs := []rbac.Attributes{migrationSettingsRequirement()}
			var zoneWrite, addressWrite preparedCommandWrite
			var addressItem *ApplyResult
			var validation error
			var z *public.DNSZone
			switch {
			case c.Error != "":
				validation = errConflict(c.Error, "Preview the cutover again")
			case len(c.TargetNameservers) == 0:
				validation = errUnavailable("reviewed cutover has no target nameservers")
			default:
				z, validation = migrationZoneFrom(ctx, tx, r, r.Spec.Zones[i])
			}
			if validation == nil && z.ResourceVersion != c.ResourceVersion {
				validation = errPrecondition(c.ResourceVersion, z.ResourceVersion)
			}
			if validation == nil {
				k, _ := kindByKind("DNSZone")
				body, _ := objectMap(k, z)
				attrs = append(attrs, commandWriteAttributes(k, store.KeyOf(z), body, z)...)
				if c.AddressRecordSet != nil {
					sets, e := currentZoneRecordSets(ctx, tx, z.Name)
					validation = e
					var rs *public.DNSRecordSet
					if validation == nil {
						rs, validation = s.migrationAddressesFrom(ctx, tx, r, z, fresh.Addresses, sets, false)
					}
					if validation == nil && !reflect.DeepEqual(rs, c.AddressRecordSet) {
						validation = errConflict("Generated nameserver records changed", "Preview the cutover again")
					}
					if validation == nil {
						k, _ := kindByKind("DNSRecordSet")
						body, e := objectMap(k, rs)
						validation = e
						if validation == nil {
							var a []rbac.Attributes
							addressWrite, a, validation = s.prepareMigrationDocument(ctx, tx, body, rs.ResourceVersion, rs.ResourceVersion == "")
							attrs = append(attrs, a...)
						}
					}
					addressItem = &ApplyResult{Kind: "DNSRecordSet", Namespace: c.AddressRecordSet.Namespace, Name: c.AddressRecordSet.Name}
				}
				if validation == nil {
					z.Spec.Nameservers = append([]string{}, c.TargetNameservers...)
					if z.Spec.SOA == nil {
						z.Spec.SOA = &public.SOASpec{}
					}
					z.Spec.SOA.MName = c.TargetNameservers[0]
					body, e := objectMap(k, z)
					validation = e
					if validation == nil {
						zoneWrite, _, validation = s.prepareMigrationDocument(ctx, tx, body, c.ResourceVersion, false)
					}
				}
			}
			return migrationPlanFor(tx, attrs, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if validation != nil {
					item = migrationFailure(item, validation)
					addressItem = nil
				} else {
					if addressItem != nil {
						wr, e := addressWrite.apply(ctx, tx)
						if e != nil {
							return e
						}
						addressItem.Result = wr.result
						addressItem.ResourceVersion = wr.obj.GetResourceVersion()
					}
					wr, e := zoneWrite.apply(ctx, tx)
					if e != nil {
						return e
					}
					item.Result = wr.result
					item.ResourceVersion = wr.obj.GetResourceVersion()
					tracked := &r.Spec.Zones[i]
					tracked.UID = string(wr.obj.GetUID())
					tracked.Nameservers = append([]string{}, c.TargetNameservers...)
					tracked.Primary = c.TargetNameservers[0]
				}
				last := &r.Spec.Cutovers[len(r.Spec.Cutovers)-1]
				if addressItem != nil {
					last.Items = append(last.Items, migrationItem(*addressItem))
				}
				last.Items = append(last.Items, migrationItem(item))
				if i == len(reviewed.Zones)-1 {
					// The final outcome and terminal report share this commit.
					// A later publication must not leave all effects durable
					// while a separate completion command can still be refused.
					last.Status = "completed"
					now := metav1.NewTime(s.opts.Now())
					last.FinishedAt = &now
					for _, item := range last.Items {
						if item.Result == ResultError {
							last.Status = "partial"
						}
					}
				}
				return saveCurrentMigration(ctx, tx, r)
			}, func() {
				run = r
				if addressItem != nil {
					result.Items = append(result.Items, *addressItem)
				}
				result.Items = append(result.Items, item)
			}), nil
		})
	}

	next, err := s.admitMigration(ctx, program)
	if err != nil {
		return nil, err
	}
	for i := 0; i < program.Len(); i++ {
		if err := next(ctx); err != nil {
			if i == 0 {
				return nil, storeError(err)
			}
			result.JournalError = "Cutover stopped before further writes: " + truncateMigrationError(err.Error())
			break
		}
	}
	return result, nil
}
