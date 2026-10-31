package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func migrationProgramStore(t *testing.T) *sqlite.Store {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "migration.db"), sqlite.Options{Scheme: scheme, Validator: testValidator(t), InternalKinds: []store.Kind{internal.KindMigrationRun}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
func TestMigrationCommandProgramDetachesReviewAndAcceptsOnlyCommittedResults(t *testing.T) {
	h := newHarness(t)
	st := migrationProgramStore(t)
	input := migrationPlan("original")
	program, run, result, err := h.srv.currentImportProgram(t.Context(), &input)
	if err != nil {
		t.Fatal(err)
	}
	input.Objects[0]["metadata"].(map[string]any)["name"] = "changed-after-admission"
	input.Objects[0]["spec"].(map[string]any)["zoneName"] = "foreign.test."
	input.Settings["blocking"] = map[string]any{"enabled": false}
	execute := func(i int, fail bool) error {
		var plan *MigrationCommandPlan
		err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var err error
			plan, err = program.Prepare(ctx, tx, i)
			if err != nil {
				return err
			}
			if !plan.PreparedFor(tx) || plan.PreparedFor(new(sqlite.CommandTx)) {
				t.Fatal("plan lost transaction identity")
			}
			requirements := plan.Requirements()
			requirements[0].Resource = "tokens"
			if plan.Requirements()[0].Resource != "settings" {
				t.Fatal("mutable requirements")
			}
			if fail {
				if err := tx.RequireCommitCheck(func(context.Context) error { return errUnavailable("final admission failed") }); err != nil {
					return err
				}
			}
			return plan.Apply(ctx, tx)
		})
		if err == nil {
			plan.Committed()
		}
		if plan != nil {
			plan.Release()
		}
		return err
	}
	if err := execute(0, false); err != nil {
		t.Fatal(err)
	}
	if err := execute(1, true); err == nil {
		t.Fatal("final rejection accepted")
	}
	if len(result.Items) != 0 {
		t.Fatal("uncommitted result exposed", result)
	}
	var absent public.DNSZone
	if !errors.Is(st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "original"}, &absent), store.ErrNotFound) {
		t.Fatal("rejected step committed")
	}
	// Unit-level replay constructs a new transaction only to inspect the sealed
	// payload. The production executor terminalizes its program after any error.
	if err := execute(1, false); err != nil {
		t.Fatal(err)
	}
	var z public.DNSZone
	if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "original"}, &z); err != nil || z.Spec.ZoneName != "original.test." {
		t.Fatal("caller changed the admitted plan", err, z.Spec)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "original" || (*run).Spec.Items[0].Result != ResultCreated {
		t.Fatal("committed result missing", result)
	}
}
func TestMigrationCommandMissingCapabilityNeverUsesLegacyWriters(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.ObjectCommand = func(context.Context, store.Key, func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error {
			t.Fatal("fell through to generic object writer")
			return nil
		}
	})
	if _, err := h.srv.applyCurrentMigration(t.Context(), func() *importers.Plan { p := migrationPlan("unsafe"); return &p }()); err == nil {
		t.Fatal("missing current migration capability accepted")
	}
	list, err := h.st.List(t.Context(), internal.KindMigrationRun, store.ListOptions{})
	if err != nil || len(list.Items) != 0 {
		t.Fatal("legacy journal written", err)
	}
}
