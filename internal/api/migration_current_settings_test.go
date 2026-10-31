package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type migrationSettingsPlanner struct {
	host *config.Config
}

func (p migrationSettingsPlanner) Settings(context.Context) (*config.Config, error) {
	return nil, errors.New("import must read transaction-local canonical state")
}
func (p migrationSettingsPlanner) WriteSettings(context.Context, *config.Config, []string) error {
	return errors.New("import must never nest an ordinary settings writer")
}
func (p migrationSettingsPlanner) PrepareSettings(ctx context.Context, tx *sqlite.CommandTx, patch map[string]any) (*CanonicalSettingsPlan, error) {
	return PrepareCanonicalSettingsPatch(ctx, tx, p.host, patch)
}

func migrationSettingsStep(ctx context.Context, st *sqlite.Store, program *MigrationProgram, index int, beforeApply func(*sqlite.CommandTx) error) error {
	var plan *MigrationCommandPlan
	err := st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		plan, err = program.Prepare(ctx, tx, index)
		if err != nil {
			return err
		}
		if beforeApply != nil {
			if err := beforeApply(tx); err != nil {
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

func migrationSettingsFixture(t *testing.T) (*Server, *sqlite.Store, *config.Config) {
	t.Helper()
	h := newHarness(t)
	st := migrationProgramStore(t)
	if _, err := st.Apply(t.Context(), &public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	host := h.srv.config()
	h.srv.opts.CanonicalSettings = migrationSettingsPlanner{host: host}
	return h.srv, st, host
}

func migrationSettingsRead(t *testing.T, st *sqlite.Store, host *config.Config) (*public.Kudurru, *config.Config) {
	t.Helper()
	var obj *public.Kudurru
	var cfg *config.Config
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		obj, _, cfg, err = currentSettings(ctx, tx, host)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return obj, cfg
}

func TestMigrationSettingsMergeCurrentSelectionResetAndRestart(t *testing.T) {
	s, st, host := migrationSettingsFixture(t)
	input := importers.Plan{Objects: []map[string]any{}, Settings: map[string]any{
		"cache":     map[string]any{"maxEntries": nil},
		"upstreams": map[string]any{"conditional": map[string]any{"remove.test.": nil, "new.test.": "default"}},
		"blocking":  map[string]any{"initStrategy": "fast"},
		"listeners": map[string]any{"api": map[string]any{"address": ":1"}},
	}, Selection: &importers.Selection{Objects: []string{}, Settings: []string{"cache", "upstreams", "blocking"}}}
	program, run, result, err := s.currentImportProgram(t.Context(), &input)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrationSettingsStep(t.Context(), st, program, 0, nil); err != nil {
		t.Fatal(err)
	}
	// A different admitted command changes current state between journal
	// initialization and settings completion. The copied patch must merge it.
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		plan, err := PrepareCanonicalSettingsPatch(ctx, tx, host, map[string]any{"cache": map[string]any{"maxEntries": 77}, "upstreams": map[string]any{"conditional": map[string]any{"keep.test.": "default", "remove.test.": "default"}}})
		if err != nil {
			return err
		}
		return plan.Apply(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}
	input.Settings["blocking"].(map[string]any)["initStrategy"] = "wrong-after-admission"
	if err := migrationSettingsStep(t.Context(), st, program, 1, nil); err != nil {
		t.Fatal(err)
	}
	_, cfg := migrationSettingsRead(t, st, host)
	defaultConfig, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.MaxEntries != defaultConfig.Cache.MaxEntries || cfg.Blocking.InitStrategy != "fast" || len(cfg.Upstreams.Conditional) != 2 || cfg.Upstreams.Conditional["keep.test."] != "default" || cfg.Upstreams.Conditional["new.test."] != "default" {
		t.Fatal("patch lost current map/reset/selection semantics", cfg.Cache, cfg.Upstreams.Conditional, cfg.Blocking)
	}
	if result.Settings == nil || result.Settings.Persisted == nil || !*result.Settings.Persisted || len(result.Settings.RestartRequired) != 1 || result.Settings.RestartRequired[0] != "blocking.initStrategy" || result.SettingsError != "" {
		t.Fatal("incomplete settings response", result)
	}
	if (*run).Spec.Status != "completed" || (*run).Spec.SettingsPersisted == nil || !*(*run).Spec.SettingsPersisted || (*run).Spec.Items[2].Result != "skipped" || !strings.Contains((*run).Spec.Items[0].Hint, "blocking.initStrategy") {
		t.Fatal("incomplete durable settings report", (*run).Spec)
	}
}

func TestMigrationSettingsValidationErrorsAreDurableWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		patch     map[string]any
		lock      bool
	}{
		{"invalid", KeyInvalid, map[string]any{"cache": map[string]any{"maxEntries": -1}}, false},
		{"unknown", KeyInvalid, map[string]any{"unknown": true}, false},
		{"host", KeyHostOnly, map[string]any{"listeners": map[string]any{"api": map[string]any{"address": ":1"}}}, false},
		{"node", KeyInvalid, map[string]any{"log": map[string]any{"level": "debug"}}, false},
		{"locked", KeyEnvLocked, map[string]any{"cache": map[string]any{"maxEntries": 77}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, host := migrationSettingsFixture(t)
			if tc.lock {
				host.EnvLocked = []string{"cache.maxEntries"}
			}
			before, _ := migrationSettingsRead(t, st, host)
			input := importers.Plan{Settings: tc.patch}
			program, run, result, err := s.currentImportProgram(t.Context(), &input)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < program.Len(); i++ {
				if err := migrationSettingsStep(t.Context(), st, program, i, nil); err != nil {
					t.Fatal(err)
				}
			}
			after, _ := migrationSettingsRead(t, st, host)
			if after.ResourceVersion != before.ResourceVersion || result.Settings != nil || result.SettingsError == "" || (*run).Spec.Status != "partial" || (*run).Spec.Items[0].ErrorKey != tc.key || (*run).Spec.Items[0].Result != ResultError {
				t.Fatal("validation changed settings or lost durable rejection", (*run).Spec, result)
			}
		})
	}
}

func TestMigrationSettingsJournalFailureAndFinalRejectionRollBack(t *testing.T) {
	for _, journalFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "final-check", true: "journal-write"}[journalFailure], func(t *testing.T) {
			s, st, host := migrationSettingsFixture(t)
			input := migrationPlan("prior-success")
			input.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
			program, run, result, err := s.currentImportProgram(t.Context(), &input)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := migrationSettingsStep(t.Context(), st, program, i, nil); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := migrationSettingsRead(t, st, host)
			err = migrationSettingsStep(t.Context(), st, program, 2, func(tx *sqlite.CommandTx) error {
				if journalFailure {
					return tx.Delete(t.Context(), store.KeyOf(*run), store.DeleteOptions{Manager: store.ManagerSystem})
				}
				return tx.RequireCommitCheck(func(context.Context) error { return errors.New("final rejection") })
			})
			if err == nil {
				t.Fatal("settings completion was not rejected")
			}
			after, _ := migrationSettingsRead(t, st, host)
			var durable internal.MigrationRun
			if err := st.Get(t.Context(), store.KeyOf(*run), &durable); err != nil {
				t.Fatal(err)
			}
			if after.ResourceVersion != before.ResourceVersion || result.Settings != nil || result.SettingsError != "" || durable.Spec.Status != "running" || durable.Spec.Items[0].Result != ResultCreated || durable.Spec.Items[1].Result != "pending" {
				t.Fatal("failed completion escaped rollback or invented outcomes", durable.Spec, result)
			}
			var prior public.DNSZone
			if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "prior-success"}, &prior); err != nil {
				t.Fatal("prior successful step was lost", err)
			}
		})
	}
}

func TestCanonicalSettingsPlanRejectsAnotherTransactionAndReuse(t *testing.T) {
	_, st, host := migrationSettingsFixture(t)
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		plan, err := PrepareCanonicalSettingsPatch(ctx, tx, host, map[string]any{"cache": map[string]any{"maxEntries": 77}})
		if err != nil {
			return err
		}
		if err := plan.Apply(ctx, new(sqlite.CommandTx)); err == nil {
			t.Fatal("plan accepted foreign transaction")
		}
		if err := plan.Apply(ctx, tx); err != nil {
			return err
		}
		if err := plan.Apply(ctx, tx); err == nil {
			t.Fatal("plan accepted reuse")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
