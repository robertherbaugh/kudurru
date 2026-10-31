package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func currentCutoverFixture(t *testing.T, names ...string) (*Server, *sqlite.Store, *internal.MigrationRun) {
	t.Helper()
	s := newHarness(t).srv
	st := migrationProgramStore(t)
	input := migrationPlan(names...)
	for _, doc := range input.Objects {
		doc["spec"].(map[string]any)["nameservers"] = []string{"old.dns.test."}
	}
	program, run, result, err := s.currentImportProgram(t.Context(), &input)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < program.Len(); i++ {
		if err := migrationSettingsStep(t.Context(), st, program, i, nil); err != nil {
			t.Fatal(err)
		}
	}
	if result.JournalError != "" || (*run).Spec.Status != "completed" {
		t.Fatal("seed import did not complete")
	}
	return s, st, *run
}

func currentCutoverRead(t *testing.T, st *sqlite.Store, id string) *internal.MigrationRun {
	t.Helper()
	var run internal.MigrationRun
	if err := st.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: id}, &run); err != nil {
		t.Fatal(err)
	}
	return &run
}

func TestCurrentCutoverTerminalReportCommitsWithLastOutcome(t *testing.T) {
	for _, mode := range []string{"single-zone", "multi-zone", "first-error", "last-error", "unchanged", "VIP"} {
		t.Run(mode, func(t *testing.T) {
			names := []string{"first", "last"}
			if mode == "single-zone" {
				names = names[:1]
			}
			s, st, run := currentCutoverFixture(t, names...)
			wantStatus := "completed"
			wantResults := map[string]string{}
			for _, name := range names {
				wantResults[name] = ResultUpdated
			}
			errorZone := ""
			if mode == "first-error" || mode == "last-error" {
				errorZone = "first"
				if mode == "last-error" {
					errorZone = "last"
				}
				wantStatus = "partial"
				wantResults[errorZone] = ResultError
			}
			ns, addresses := []string{"dns.kudurru.test."}, []string(nil)
			if mode == "unchanged" {
				ns = []string{"old.dns.test."}
				for _, name := range names {
					wantResults[name] = ResultUnchanged
				}
			}
			if mode == "VIP" {
				ns, addresses = nil, []string{"192.0.2.53", "2001:db8::53"}
			}
			preview, err := s.previewMigrationCutoverFrom(t.Context(), st, run, ns, addresses, false)
			if err != nil {
				t.Fatal(err)
			}
			s.opts.MigrationCommand = func(_ context.Context, p *MigrationProgram) (MigrationNext, error) {
				index := 0
				return func(ctx context.Context) error {
					err := migrationSettingsStep(ctx, st, p, index, nil)
					index++
					if err != nil {
						return err
					}
					// Inspect actual durable state after EVERY commit, not merely
					// the final API response. No publication or later command is
					// needed to finish a report whose outcomes all committed.
					last := currentCutoverRead(t, st, run.Name).Spec.Cutovers[0]
					zones := 0
					for _, item := range last.Items {
						if item.Kind == "DNSZone" {
							zones++
						}
					}
					if zones == len(names) {
						if last.Status != wantStatus || last.FinishedAt == nil {
							t.Fatalf("all zone outcomes committed without terminal report: status=%q finished=%v", last.Status, last.FinishedAt != nil)
						}
					} else if last.Status != "running" || last.FinishedAt != nil {
						t.Fatal("report became terminal before all outcomes committed")
					}
					if index == 1 && errorZone != "" {
						// An independent command removes a reviewed zone after
						// journal initialization, before its outcome is prepared.
						return st.Delete(ctx, store.Key{Kind: "DNSZone", Name: errorZone}, store.DeleteOptions{Manager: store.ManagerAPI})
					}
					return nil
				}, nil
			}
			result, err := s.applyCurrentCutover(t.Context(), run.Name, preview)
			if err != nil || result.JournalError != "" {
				t.Fatal("cutover failed", err, result)
			}
			for _, item := range result.Items {
				if item.Kind == "DNSZone" && item.Result != wantResults[item.Name] {
					t.Fatalf("zone %s result=%s want=%s", item.Name, item.Result, wantResults[item.Name])
				}
			}
			last := currentCutoverRead(t, st, run.Name).Spec.Cutovers[0]
			wantItems := len(names)
			if mode == "VIP" {
				wantItems *= 2
				for _, zone := range preview.Zones {
					var records public.DNSRecordSet
					if err := st.Get(t.Context(), store.KeyOf(zone.AddressRecordSet), &records); err != nil || len(records.Spec.Endpoints) != 2 {
						t.Fatal("terminal VIP report lost address effects", err)
					}
				}
			}
			if len(result.Items) != wantItems || len(last.Items) != wantItems {
				t.Fatal("missing durable/exposed outcomes")
			}
		})
	}
}

func TestCurrentCutoverLastRejectionPreservesEarlierOutcomes(t *testing.T) {
	for _, mode := range []string{"refused-command", "final-check", "journal-write"} {
		t.Run(mode, func(t *testing.T) {
			s, st, run := currentCutoverFixture(t, "first", "last")
			preview, err := s.previewMigrationCutoverFrom(t.Context(), st, run, nil, []string{"192.0.2.53"}, false)
			if err != nil {
				t.Fatal(err)
			}
			var before public.DNSZone
			if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "last"}, &before); err != nil {
				t.Fatal(err)
			}
			var prior *internal.MigrationRun
			s.opts.MigrationCommand = func(_ context.Context, p *MigrationProgram) (MigrationNext, error) {
				index := 0
				stopped := false
				return func(ctx context.Context) error {
					if stopped {
						t.Fatal("refused command replayed")
					}
					stopped = true
					var reject func(*sqlite.CommandTx) error
					if index == 2 {
						prior = currentCutoverRead(t, st, run.Name)
						if mode == "refused-command" {
							return errUnavailable("original current publication unavailable")
						}
						reject = func(tx *sqlite.CommandTx) error {
							if mode == "journal-write" {
								return tx.Delete(ctx, store.KeyOf(prior), store.DeleteOptions{Manager: store.ManagerSystem})
							}
							return tx.RequireCommitCheck(func(context.Context) error { return errUnavailable("final admission expired") })
						}
					}
					err := migrationSettingsStep(ctx, st, p, index, reject)
					if err == nil {
						index++
						stopped = false
					}
					return err
				}, nil
			}
			result, err := s.applyCurrentCutover(t.Context(), run.Name, preview)
			if err != nil || result.JournalError == "" || len(result.Items) != 2 {
				t.Fatal("refusal lost partial result", err, result)
			}
			after := currentCutoverRead(t, st, run.Name)
			if prior == nil || !reflect.DeepEqual(prior, after) || after.Spec.Cutovers[0].Status != "running" || after.Spec.Cutovers[0].FinishedAt != nil {
				t.Fatal("refused last step changed its journal or claimed completion")
			}
			var last public.DNSZone
			if err := st.Get(t.Context(), store.KeyOf(&before), &last); err != nil || !reflect.DeepEqual(before, last) {
				t.Fatal("refused last step changed zone", err)
			}
			var absent public.DNSRecordSet
			if !errors.Is(st.Get(t.Context(), store.KeyOf(preview.Zones[1].AddressRecordSet), &absent), store.ErrNotFound) {
				t.Fatal("refused last step committed VIP addresses")
			}
			var first public.DNSZone
			if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "first"}, &first); err != nil || first.Spec.SOA.MName != preview.Zones[0].TargetNameservers[0] {
				t.Fatal("earlier zone effect was lost", err)
			}
			var records public.DNSRecordSet
			if err := st.Get(t.Context(), store.KeyOf(preview.Zones[0].AddressRecordSet), &records); err != nil {
				t.Fatal("earlier VIP effect was lost", err)
			}
		})
	}
}
