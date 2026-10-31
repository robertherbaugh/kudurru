package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func importSettingsJournal(t *testing.T, f *servingCommandFixture, s *api.Server, id string) api.MigrationRun {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for {
		journal := migrationDecode[api.MigrationRun](t, migrationServingRequest(t, f, s, http.MethodGet, "/api/v1/imports/runs/"+id, nil, http.StatusOK))
		if journal.Data.Status != "running" {
			return journal
		}
		if time.Now().After(until) {
			t.Fatal("settings job did not finish", journal)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServingImportSettingsSynchronousAndBackground(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, objects := range []bool{false, true} {
			name := map[bool]string{false: "sync", true: "background"}[background] + map[bool]string{false: "/settings-only", true: "/object-and-settings"}[objects]
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				s := migrationServingAPI(t, f, nil)
				p := migrationServingPlan()
				if objects {
					p = migrationServingPlan("settings-zone")
				}
				p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}, "blocking": map[string]any{"initStrategy": "fast"}}
				var id string
				if background {
					started := migrationDecode[api.MigrationRun](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/start", p, http.StatusAccepted))
					id = started.ID
				} else {
					result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
					id = result.RunID
					if result.JournalError != "" || result.SettingsError != "" || result.Settings == nil || result.Settings.Persisted == nil || !*result.Settings.Persisted || len(result.Settings.RestartRequired) != 1 || result.Settings.RestartRequired[0] != "blocking.initStrategy" {
						t.Fatal("missing synchronous settings receipt", result)
					}
				}
				journal := importSettingsJournal(t, f, s, id)
				if journal.Data.Status != "completed" || journal.Data.SettingsPersisted == nil || !*journal.Data.SettingsPersisted || journal.Data.SettingsError != "" || journal.Data.Items[len(p.Objects)].Result != api.ResultUpdated || !strings.Contains(journal.Data.Items[len(p.Objects)].Hint, "blocking.initStrategy") {
					t.Fatal("incoherent durable settings report", journal)
				}
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					cfg, err := f.s.Settings(r.Context())
					if err != nil || cfg.Cache.MaxEntries != 77 || cfg.Blocking.InitStrategy != "fast" {
						t.Fatal("complete publication missing imported settings", err)
					}
				})).ServeHTTP(httptest.NewRecorder(), f.request())
				if objects {
					var zone public.DNSZone
					if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "settings-zone"}, &zone); err != nil || journal.Data.Items[0].Result != api.ResultCreated {
						t.Fatal("object step was lost", err, journal)
					}
				}
			})
		}
	}
}

// importSettingsHook inserts only deterministic transaction-local test work.
// Production still prepares and applies the exact canonical sealed plan.
type importSettingsHook struct {
	*servingRuntime
	afterPrepare func(*sqlite.CommandTx) error
}

func (h importSettingsHook) PrepareSettings(ctx context.Context, tx *sqlite.CommandTx, patch map[string]any) (*api.CanonicalSettingsPlan, error) {
	plan, err := h.servingRuntime.PrepareSettings(ctx, tx, patch)
	if h.afterPrepare != nil {
		if checkErr := h.afterPrepare(tx); checkErr != nil {
			return nil, checkErr
		}
	}
	return plan, err
}

func TestServingImportSettingsFinalCredentialExpiryIncludesNoopAndValidation(t *testing.T) {
	for _, mode := range []string{"write", "noop", "validation"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			migrationShortCredential(t, f)
			s := migrationServingAPI(t, f, nil)
			p := migrationServingPlan("prior-settings-success")
			p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
			switch mode {
			case "noop":
				normal := migrationServingPlan()
				normal.Settings = p.Settings
				migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", normal, http.StatusOK)
			case "validation":
				p.Settings = map[string]any{"cache": map[string]any{"maxEntries": -1}}
			}
			var before public.Kudurru
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &before); err != nil {
				t.Fatal(err)
			}
			hook := importSettingsHook{servingRuntime: f.s, afterPrepare: func(tx *sqlite.CommandTx) error {
				// This check is registered before raw-proof reauthorization's
				// final check, and runs only after all writes have been staged.
				return tx.RequireCommitCheck(func(context.Context) error {
					f.offset.Store(int64(f.expires.Sub(f.now)))
					return nil
				})
			}}
			// Mount the actual runtime planner through its optional interface.
			s = importSettingsAPI(t, f, hook)
			result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
			if result.JournalError == "" || result.Settings != nil || result.SettingsError != "" || len(result.Items) != 1 || result.Items[0].Result != api.ResultCreated {
				t.Fatal("expired completion exposed a result", result)
			}
			var after public.Kudurru
			if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&before), &after); err != nil || after.ResourceVersion != before.ResourceVersion {
				t.Fatal("final expiry changed settings", err)
			}
			var journal internal.MigrationRun
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: result.RunID}, &journal); err != nil || journal.Spec.Status != "running" || journal.Spec.Items[0].Result != api.ResultCreated || journal.Spec.Items[1].Result != "pending" {
				t.Fatal("expired step changed durable outcomes", err, journal.Spec)
			}
		})
	}
}

func TestServingImportSettingsOriginalCredentialRevocationStopsCompletion(t *testing.T) {
	f := newServingCommandFixture(t)
	id, ok := tokens.Parse(f.token)
	if !ok {
		t.Fatal("token")
	}
	command := func(ctx context.Context, program *api.MigrationProgram) (api.MigrationNext, error) {
		next, err := f.s.MigrationCommand(ctx, program)
		n := 0
		return func(ctx context.Context) error {
			n++
			if n == program.Len() {
				if err := f.s.opts.Store.Delete(ctx, store.Key{Kind: "APIToken", Name: id}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					return err
				}
			}
			return next(ctx)
		}, err
	}
	s := migrationServingAPI(t, f, command)
	p := migrationServingPlan("before-revocation")
	p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
	var before public.Kudurru
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &before); err != nil {
		t.Fatal(err)
	}
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
	var after public.Kudurru
	if result.JournalError == "" || result.Settings != nil || len(result.Items) != 1 || result.Items[0].Result != api.ResultCreated {
		t.Fatal("revoked original proof changed settings", result)
	}
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&before), &after); err != nil || after.ResourceVersion != before.ResourceVersion {
		t.Fatal("revoked completion mutated settings", err)
	}
}

func TestServingImportSettingsEnterpriseFinalEntitlementIncludesNoop(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	f.patch(t, `{"branding":{"name":"Original"}}`, http.StatusOK)
	commandFixture := &servingCommandFixture{s: f.s, http: f.adapter, token: f.token}
	for _, name := range []string{"Changed", "Original"} {
		s := migrationServingAPI(t, commandFixture, nil)
		p := migrationServingPlan()
		p.Settings = map[string]any{"branding": map[string]any{"name": name}}
		var before public.Kudurru
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &before); err != nil {
			t.Fatal(err)
		}
		script := &brandingCommandClock{at: f.base.Add(time.Hour + license.Grace)}
		f.final.Store(script)
		w := migrationServingRequest(t, commandFixture, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK)
		f.final.Store(nil)
		result := migrationDecode[api.MigrationResult](t, w)
		var after public.Kudurru
		if script.calls.Load() < 2 || result.JournalError == "" || result.Settings != nil || result.SettingsError != "" {
			t.Fatal("final original entitlement was not checked", script.calls.Load(), result)
		}
		if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&before), &after); err != nil || after.ResourceVersion != before.ResourceVersion {
			t.Fatal("final entitlement failure changed settings", err)
		}
	}
}

func importSettingsAPI(t *testing.T, f *servingCommandFixture, settings api.CanonicalSettings) *api.Server {
	t.Helper()
	s, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		ObjectCommand: f.s.ObjectCommand, ZoneCommand: f.s.ZoneCommand, MigrationCommand: f.s.MigrationCommand,
		CanonicalSettings: settings, MigrationReads: servingbuild.NewMigrationReads(), ObjectReads: servingbuild.NewObjectReads(),
		Authenticate: f.http.Middleware, Authorizer: f.http, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestServingImportSettingsObjectFailureLeavesSettingsUnapplied(t *testing.T) {
	f := newServingCommandFixture(t)
	s := migrationServingAPI(t, f, nil)
	p := migrationServingPlan("before-invalid-object", "invalid-object")
	p.Objects[1]["spec"].(map[string]any)["zoneName"] = ""
	p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
	var before public.Kudurru
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &before); err != nil {
		t.Fatal(err)
	}
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
	journal := importSettingsJournal(t, f, s, result.RunID)
	if result.Settings != nil || result.SettingsError != "" || result.JournalError != "" || len(result.Items) != 2 || journal.Data.Status != "partial" || journal.Data.Items[0].Result != api.ResultCreated || journal.Data.Items[1].Result != api.ResultError || journal.Data.Items[2].Result != "not_applied" || journal.Data.SettingsPersisted != nil {
		t.Fatal("object rejection changed partial semantics", result, journal)
	}
	var after public.Kudurru
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&before), &after); err != nil || after.ResourceVersion != before.ResourceVersion {
		t.Fatal("object rejection changed selected settings", err)
	}
}

func TestServingImportSettingsLicenseRejectionIsDurable(t *testing.T) {
	f := newServingCommandFixture(t)
	s := migrationServingAPI(t, f, nil)
	p := migrationServingPlan()
	p.Settings = map[string]any{"branding": map[string]any{"name": "Unlicensed"}}
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusOK))
	journal := importSettingsJournal(t, f, s, result.RunID)
	if result.SettingsError == "" || result.JournalError != "" || result.Settings != nil || journal.Data.Status != "partial" || journal.Data.Items[0].ErrorKey != api.KeyLicense || journal.Data.Items[0].Result != api.ResultError || journal.Data.SettingsPersisted != nil {
		t.Fatal("license rejection was not journaled", result, journal)
	}
}

func TestServingImportSettingsPermissionRejectionDoesNotCreateJournal(t *testing.T) {
	f := newServingCommandFixture(t)
	id, ok := tokens.Parse(f.token)
	if !ok {
		t.Fatal("token")
	}
	var token internal.APIToken
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
		t.Fatal(err)
	}
	token.Spec.Scopes = []string{"viewer"}
	if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	s := migrationServingAPI(t, f, nil)
	p := migrationServingPlan()
	p.Settings = map[string]any{"cache": map[string]any{"maxEntries": 77}}
	migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", p, http.StatusForbidden)
	list, err := f.s.opts.Store.List(t.Context(), internal.KindMigrationRun, store.ListOptions{})
	if err != nil || len(list.Items) != 0 {
		t.Fatal("denied settings import created a journal", err, list)
	}
}
