package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestServingCutoverLastFinalExpiryPreservesEarlierOutcome(t *testing.T) {
	for _, noOp := range []bool{false, true} {
		t.Run(map[bool]string{false: "VIP", true: "unchanged"}[noOp], func(t *testing.T) {
			f := newServingCommandFixture(t)
			migrationShortCredential(t, f)
			normal := httptest.NewServer(servingMutationRoutes(migrationServingAPI(t, f, nil)))
			defer normal.Close()
			// Seed through the existing atomic adoption path to isolate this
			// test from independent multi-step import completion/publication.
			adoptionSeed(t, f, "first", "last")
			reviewed := adoptionSelection(t, f, normal)
			imported := adoptionJSON[api.MigrationResult](t, adoptionHTTP(t, f, normal, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusOK))
			if imported.JournalError != "" || len(imported.Items) != 2 {
				t.Fatal("seed adoption failed")
			}
			path := "/imports/runs/" + imported.RunID + "/decommission"
			target := map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}
			if noOp {
				target = map[string]any{"nameservers": []string{"old.test."}}
			}
			preview := adoptionJSON[api.MigrationCutoverPlan](t, adoptionHTTP(t, f, normal, http.MethodPost, path+"/preview", target, http.StatusOK))
			var before public.DNSZone
			lastKey := store.Key{Kind: "DNSZone", Name: preview.Zones[1].Name}
			if err := f.s.opts.Store.Get(t.Context(), lastKey, &before); err != nil {
				t.Fatal(err)
			}
			var attempts, clockCalls atomic.Int32
			command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
				next, err := f.s.MigrationCommand(ctx, p)
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context) error {
					if attempts.Add(1) == 3 {
						adoptionWait(t, f)
						// Original proof passes preparation/reauthorization,
						// then expires at the actual final commit check.
						f.s.opts.Now = func() time.Time {
							if clockCalls.Add(1) <= 3 {
								return f.now
							}
							return f.expires
						}
					}
					return next(ctx)
				}, nil
			}
			failing := httptest.NewServer(servingMutationRoutes(migrationServingAPI(t, f, command)))
			defer failing.Close()
			// Exactly one HTTP mutation; a refused command is never replayed.
			outcome := adoptionJSON[api.MigrationResult](t, adoptionHTTP(t, f, failing, http.MethodPost, path, preview, http.StatusOK))
			wantItems := 2
			if noOp {
				wantItems = 1
			}
			if !strings.Contains(outcome.JournalError, "unauthenticated") || len(outcome.Items) != wantItems || attempts.Load() != 3 || clockCalls.Load() <= 3 {
				t.Fatalf("final expiry outcome: error=%q items=%d attempts=%d clockCalls=%d", outcome.JournalError, len(outcome.Items), attempts.Load(), clockCalls.Load())
			}
			var after public.DNSZone
			if err := f.s.opts.Store.Get(t.Context(), lastKey, &after); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("final expiry changed the last zone", err)
			}
			if rs := preview.Zones[1].AddressRecordSet; rs != nil {
				var absent public.DNSRecordSet
				if !errors.Is(f.s.opts.Store.Get(t.Context(), store.KeyOf(rs), &absent), store.ErrNotFound) {
					t.Fatal("final expiry committed last VIP records")
				}
			}
			var run internal.MigrationRun
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: imported.RunID}, &run); err != nil {
				t.Fatal(err)
			}
			last := run.Spec.Cutovers[0]
			if last.Status != "running" || last.FinishedAt != nil || len(last.Items) != wantItems {
				t.Fatal("final expiry claimed terminal or uncommitted outcomes")
			}
			var first public.DNSZone
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: preview.Zones[0].Name}, &first); err != nil || first.Spec.SOA.MName != preview.Zones[0].TargetNameservers[0] {
				t.Fatal("final expiry lost the earlier zone", err)
			}
			wantResult := api.ResultUpdated
			if noOp {
				wantResult = api.ResultUnchanged
			}
			if last.Items[wantItems-1].Result != wantResult || outcome.Items[wantItems-1].Result != wantResult {
				t.Fatal("earlier committed outcome was not preserved")
			}
			if rs := preview.Zones[0].AddressRecordSet; rs != nil {
				var records public.DNSRecordSet
				if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(rs), &records); err != nil {
					t.Fatal("final expiry lost earlier VIP records", err)
				}
			}
		})
	}
}
