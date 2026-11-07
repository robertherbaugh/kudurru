package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func adoptionCandidates(t *testing.T, st *sqlite.Store, names ...string) []MigrationAuthorityChange {
	t.Helper()
	out := make([]MigrationAuthorityChange, 0, len(names))
	for _, name := range names {
		sum := sha256.Sum256([]byte(name + ".test"))
		obj, err := st.Apply(t.Context(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("technitium-%s-test-%x", name, sum[:4]), Labels: map[string]string{"retain": "label"}, Annotations: map[string]string{"retain": "annotation"}}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: name + ".test.", Nameservers: []string{"old.test."}, SOA: &public.SOASpec{MName: "old.test.", RName: "hostmaster.test."}}}, store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			t.Fatal(err)
		}
		z := obj.(*public.DNSZone)
		out = append(out, MigrationAuthorityChange{Name: z.Name, UID: string(z.UID), ZoneName: z.Spec.ZoneName, ResourceVersion: z.ResourceVersion, Nameservers: append([]string{}, z.Spec.Nameservers...), Primary: zonePrimary(z)})
	}
	return out
}

func adoptionExecute(t *testing.T, st *sqlite.Store, program *MigrationProgram, beforeApply func(context.Context, *sqlite.CommandTx, *MigrationCommandPlan) error) error {
	t.Helper()
	var plan *MigrationCommandPlan
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		plan, err = program.Prepare(ctx, tx, 0)
		if err != nil {
			return err
		}
		if beforeApply != nil {
			if err := beforeApply(ctx, tx, plan); err != nil {
				return err
			}
		}
		return plan.Apply(ctx, tx)
	})
	if plan != nil {
		if err == nil {
			plan.Committed()
		}
		plan.Release()
	}
	return err
}

func TestCurrentAdoptionCopiedReviewAndAtomicHistory(t *testing.T) {
	h := newHarness(t)
	st := migrationProgramStore(t)
	reviewed := adoptionCandidates(t, st, "one", "two", "three", "four")
	original := reviewed[0]
	var before public.DNSZone
	key := store.Key{Kind: "DNSZone", Name: original.Name}
	if err := st.Get(t.Context(), key, &before); err != nil {
		t.Fatal(err)
	}
	program, result, err := h.srv.currentAdoptionProgram(reviewed)
	if err != nil {
		t.Fatal(err)
	}
	reviewed[0].Name = "unreviewed"
	reviewed[0].Nameservers[0] = "foreign.test."
	if program.Len() != 1 {
		t.Fatal("adoption must be one atomic command")
	}
	if err := adoptionExecute(t, st, program, func(_ context.Context, tx *sqlite.CommandTx, p *MigrationCommandPlan) error {
		if !p.PreparedFor(tx) || p.PreparedFor(new(sqlite.CommandTx)) {
			t.Fatal("transaction binding")
		}
		attrs := p.Requirements()
		if len(attrs) != 5 || attrs[0] != migrationSettingsRequirement() || attrs[1] != (rbac.Attributes{Resource: "zones", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: original.Name}, Name: original.Name}) {
			t.Fatal("incomplete permissions", attrs)
		}
		attrs[1].Resource = "settings"
		if p.Requirements()[1].Resource != "zones" {
			t.Fatal("mutable permissions")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var after public.DNSZone
	if err := st.Get(t.Context(), key, &after); err != nil {
		t.Fatal(err)
	}
	wantAnnotations := map[string]string{"retain": "annotation", public.ManagedByAnnotation: store.ManagerAPI, migrationRunAnnotation: result.RunID}
	if !reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.Status, after.Status) || !reflect.DeepEqual(before.Labels, after.Labels) || !reflect.DeepEqual(after.Annotations, wantAnnotations) || after.UID != before.UID {
		t.Fatal("adoption changed data beyond the run annotation", before, after)
	}
	var run internal.MigrationRun
	if err := st.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: result.RunID}, &run); err != nil || run.Spec.Status != "completed" || run.Spec.FinishedAt == nil || run.Spec.Source != "existing" || len(result.Items) != 4 || len(run.Spec.Items) != 4 || len(run.Spec.Zones) != 4 {
		t.Fatal("missing completed atomic journal", err, run.Spec, result)
	}
	if run.Spec.Zones[0].UID != original.UID || run.Spec.Items[0].ResourceVersion != after.ResourceVersion || run.Spec.Zones[0].OriginalPrimary != before.Spec.SOA.MName || result.Items[0].Result != ResultUpdated {
		t.Fatal("lost reviewed provenance/outcome", run.Spec, result)
	}
}

func TestCurrentAdoptionPrevalidationAndBoundsLeaveNoJournal(t *testing.T) {
	for _, mode := range []string{"empty", "too-many", "duplicate", "missing-uid", "wrong-uid", "missing-rv", "stale-rv", "foreign-manager", "already-adopted", "non-import", "secondary", "authority", "zone-name", "namespace", "capacity", "missing-zone"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			st := migrationProgramStore(t)
			reviewed := adoptionCandidates(t, st, "first", "last")
			switch mode {
			case "empty":
				reviewed = nil
			case "too-many":
				reviewed = make([]MigrationAuthorityChange, 1001)
			case "duplicate":
				reviewed[1] = reviewed[0]
			case "missing-uid":
				reviewed[1].UID = ""
			case "wrong-uid":
				reviewed[1].UID = "recreated"
			case "missing-rv":
				reviewed[1].ResourceVersion = ""
			case "stale-rv":
				reviewed[1].ResourceVersion = "0"
			case "foreign-manager", "already-adopted", "non-import", "secondary":
				var z public.DNSZone
				if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: reviewed[1].Name}, &z); err != nil {
					t.Fatal(err)
				}
				manager := store.ManagerAPI
				switch mode {
				case "foreign-manager":
					manager = store.ManagerFile("zones.yaml")
				case "already-adopted":
					z.Annotations[migrationRunAnnotation] = "other-run"
				case "non-import":
					z.Name = "handmade"
					reviewed[1].Name = z.Name
				case "secondary":
					z.Spec.Type = public.ZoneTypeSecondary
					z.Spec.Secondary = &public.SecondarySpec{Primaries: []string{"192.0.2.53:53"}}
				}
				obj, err := st.Apply(t.Context(), &z, store.ApplyOptions{Manager: manager, Force: true})
				if err != nil {
					t.Fatal(err)
				}
				reviewed[1].ResourceVersion = obj.GetResourceVersion()
				reviewed[1].UID = string(obj.GetUID())
			case "authority":
				reviewed[1].Nameservers[0] = "other.test."
			case "zone-name":
				reviewed[1].ZoneName = "other.test."
			case "namespace":
				reviewed[1].Namespace = "other"
			case "missing-zone":
				reviewed[1].Name = "absent"
			case "capacity":
				reviewed = nil
				for i := range 500 {
					reviewed = append(reviewed, adoptionCandidates(t, st, fmt.Sprintf("capacity%d", i))...)
				}
			}
			before, err := st.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := h.srv.currentAdoptionProgram(reviewed)
			if err == nil {
				err = adoptionExecute(t, st, p, nil)
			}
			if err == nil {
				t.Fatal("invalid selection accepted")
			}
			after, err := st.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("prevalidation created journal or adopted earlier zone", err)
			}
		})
	}
}

func TestCurrentAdoptionRollbackOnMutationOrFinalFailure(t *testing.T) {
	for _, mode := range []string{"mutation", "final-check"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			st := migrationProgramStore(t)
			reviewed := adoptionCandidates(t, st, "first", "last")
			p, result, err := h.srv.currentAdoptionProgram(reviewed)
			if err != nil {
				t.Fatal(err)
			}
			before, err := st.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = adoptionExecute(t, st, p, func(ctx context.Context, tx *sqlite.CommandTx, _ *MigrationCommandPlan) error {
				if mode == "final-check" {
					return tx.RequireCommitCheck(func(context.Context) error { return errUnavailable("final credential expired") })
				}
				// Force a real mutation failure after the first adoption write.
				// Even this competing staged manager transfer must roll back.
				var z public.DNSZone
				if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: reviewed[1].Name}, &z); err != nil {
					return err
				}
				_, err := tx.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerFile("zones.yaml"), Force: true})
				return err
			})
			if err == nil || result.RunID != "" || len(result.Items) != 0 {
				t.Fatal("failed transaction published success", err, result)
			}
			after, err := st.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("adoption or journal escaped rollback", err)
			}
		})
	}
}

func TestCurrentAdoptionMissingCapabilityNeverWritesLegacyJournal(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.ObjectCommand = func(context.Context, store.Key, func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error {
			t.Fatal("fell back to object writer")
			return nil
		}
	})
	st := migrationProgramStore(t)
	reviewed := adoptionCandidates(t, st, "one")
	h.do(http.MethodPost, "/api/v1/imports/adopt", "admin", map[string]any{"zones": reviewed}).want(http.StatusServiceUnavailable)
	list, err := h.st.List(t.Context(), internal.KindMigrationRun, store.ListOptions{})
	if err != nil || len(list.Items) != 0 {
		t.Fatal("legacy journal created", err)
	}
}

func TestCurrentAdoptionBoundsAggregateStoredZoneBytes(t *testing.T) {
	h := newHarness(t)
	st := migrationProgramStore(t)
	// Each object is valid and under the annotation limit; only their total
	// exceeds the command budget. Seed once to avoid a sequence of large images.
	padding := strings.Repeat("x", 250<<10)
	reviewed := make([]MigrationAuthorityChange, 0, 265)
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for i := range 265 {
			name := fmt.Sprintf("large%d", i)
			sum := sha256.Sum256([]byte(name + ".test"))
			obj, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("technitium-%s-test-%x", name, sum[:4]), Annotations: map[string]string{"padding": padding}}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: name + ".test.", Nameservers: []string{"old.test."}}}, store.ApplyOptions{Manager: store.ManagerAPI})
			if err != nil {
				return err
			}
			z := obj.(*public.DNSZone)
			reviewed = append(reviewed, MigrationAuthorityChange{Name: z.Name, UID: string(z.UID), ResourceVersion: z.ResourceVersion, ZoneName: z.Spec.ZoneName, Nameservers: append([]string{}, z.Spec.Nameservers...), Primary: zonePrimary(z)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	program, result, err := h.srv.currentAdoptionProgram(reviewed)
	if err != nil {
		t.Fatal(err)
	}
	if err := adoptionExecute(t, st, program, nil); err == nil || !strings.Contains(err.Error(), "object inventory exceeds") {
		t.Fatal("aggregate stored content was not bounded", err)
	}
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) || result.RunID != "" || len(result.Items) != 0 {
		t.Fatal("oversized inventory created adoption or journal", err, result)
	}
}
