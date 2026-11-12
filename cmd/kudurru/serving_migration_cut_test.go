package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// migrationCutReceiver borrows the fixture's actual command machinery with a
// hook between a later step's cut capture and its command. It is never
// mounted, so no running worker sees the hook.
func migrationCutReceiver(f *servingCommandFixture, hook func(context.Context, int)) *servingRuntime {
	return &servingRuntime{opts: f.s.opts, primary: f.s.primary, publisher: f.s.publisher, providers: f.s.providers, afterMigrationCut: hook}
}

func migrationSettingsApplied(t *testing.T, f *servingCommandFixture, s *api.Server, result api.MigrationResult) {
	t.Helper()
	if result.JournalError != "" || result.SettingsError != "" || result.Settings == nil || result.Settings.Persisted == nil || !*result.Settings.Persisted {
		t.Fatal("settings step did not complete", result)
	}
	journal := importSettingsJournal(t, f, s, result.RunID)
	if journal.Data.Status != "completed" || journal.Data.SettingsPersisted == nil || !*journal.Data.SettingsPersisted {
		t.Fatal("incoherent durable settings report", journal)
	}
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cfg, err := f.s.Settings(r.Context())
		if err != nil || cfg.Cache.MaxEntries != 77 {
			t.Error("complete publication missing imported settings", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
}

// A commit by another writer (in production, the zone lifecycle driver after
// the import's zone step) that lands after a later step captured its cut and
// before the step's command began supersedes that cut: the command is refused
// with serving.ErrPending before anything was authenticated, prepared or
// staged. The step must wait for the new publication and capture again, as it
// does when the cut is not yet published, instead of ending the import partial.
func TestServingMigrationStepRecapturesCutSupersededBeforeDispatch(t *testing.T) {
	f := newServingCommandFixture(t)
	var captures atomic.Int32
	receiver := migrationCutReceiver(f, func(ctx context.Context, index int) {
		if captures.Add(1) != 1 {
			return
		}
		if _, err := f.s.opts.Store.Apply(ctx, &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "concurrent", Namespace: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Error("concurrent commit", err)
			return
		}
		// Hold the step until its captured cut is visibly superseded, so the
		// command below is refused for exactly this reason.
		target := serving.TermActivation{Term: f.s.primary.driver.Status().Writer.Term, Incarnation: f.s.primary.driver.Status().Writer.Incarnation}
		for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(time.Millisecond) {
			if errors.Is(servinghttp.CheckWriter(ctx, f.s.publisher, target), serving.ErrPending) {
				return
			}
		}
		t.Error("concurrent commit did not supersede the captured cut")
	})
	s := migrationServingAPI(t, f, receiver.MigrationCommand)
	p := migrationServingPlan()
	p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
	migrationSettingsApplied(t, f, s, result)
	if captures.Load() != 2 {
		t.Fatal("settings step did not capture exactly one replacement cut", captures.Load())
	}
	var policy public.Policy
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "concurrent"}, &policy); err != nil {
		t.Fatal("concurrent commit lost", err)
	}
}

// The 5 s wait bounds only the capture of a later step's cut. A command that
// runs longer after its cut was captured (a starved or slow machine) must not
// be canceled by that wait, which used to surface as an unrelated refusal such
// as "servingauth: owner bounds exceeded".
func TestServingMigrationCutWaitDoesNotBoundTheStepCommand(t *testing.T) {
	f := newServingCommandFixture(t)
	var captures atomic.Int32
	receiver := migrationCutReceiver(f, func(ctx context.Context, index int) {
		if captures.Add(1) != 1 {
			return
		}
		select {
		case <-time.After(5500 * time.Millisecond):
		case <-ctx.Done():
		}
	})
	s := migrationServingAPI(t, f, receiver.MigrationCommand)
	p := migrationServingPlan()
	p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
	migrationSettingsApplied(t, f, s, result)
	if captures.Load() != 1 {
		t.Fatal("settings step captured a replacement cut", captures.Load())
	}
}
