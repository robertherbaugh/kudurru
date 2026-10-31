package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func migrationServingAPI(t *testing.T, f *servingCommandFixture, command api.MigrationCommand) *api.Server {
	t.Helper()
	if command == nil {
		command = f.s.MigrationCommand
	}
	s, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, ObjectCommand: f.s.ObjectCommand, ZoneCommand: f.s.ZoneCommand, MigrationCommand: command, MigrationReads: servingbuild.NewMigrationReads(), ObjectReads: servingbuild.NewObjectReads(), Authenticate: f.http.Middleware, Authorizer: f.http, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func migrationServingRequest(t *testing.T, f *servingCommandFixture, s *api.Server, method, path string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	var out *httptest.ResponseRecorder
	// Apply the actual production compound-route gate, too.
	handler := servingMutationRoutes(s)
	until := time.Now().Add(10 * time.Second)
	for {
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, "http://kudurru.test"+path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+f.token)
		request.Header.Set("Content-Type", "application/json")
		out = httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		if out.Code != http.StatusServiceUnavailable || time.Now().After(until) {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	if out.Code != want {
		t.Fatalf("%s %s: %d want %d: %s", method, path, out.Code, want, out.Body)
	}
	return out
}
func migrationServingPlan(names ...string) importers.Plan {
	p := importers.Plan{Objects: []map[string]any{}, Settings: map[string]any{}, Warnings: []string{}}
	for _, name := range names {
		p.Objects = append(p.Objects, map[string]any{"apiVersion": public.GroupVersion.String(), "kind": "DNSZone", "metadata": map[string]any{"name": name}, "spec": map[string]any{"zoneName": name + ".test.", "type": "primary", "nameservers": []string{"old.test."}, "soa": map[string]any{"mname": "old.test.", "rname": "hostmaster.test."}}})
	}
	return p
}
func migrationDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServingMigrationActualHTTPApplyAndAuthorityCutovers(t *testing.T) {
	f := newServingCommandFixture(t)
	s := migrationServingAPI(t, f, nil)
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", migrationServingPlan("migrate"), http.StatusOK))
	if result.JournalError != "" || len(result.Items) != 1 || result.Items[0].Result != api.ResultCreated {
		t.Fatalf("import: %+v", result)
	}
	path := "/api/v1/imports/runs/" + result.RunID
	for _, target := range []map[string]any{{"nameservers": []string{"dns1.kudurru.test", "dns2.kudurru.test"}}, {"nameservers": []string{}, "addresses": []string{"192.0.2.53/24", "2001:db8::53/64"}}} {
		preview := migrationDecode[api.MigrationCutoverPlan](t, migrationServingRequest(t, f, s, http.MethodPost, path+"/decommission/preview", target, http.StatusOK))
		if preview.Zones[0].Error != "" {
			t.Fatal(preview.Zones[0].Error)
		}
		result = migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, path+"/decommission", preview, http.StatusOK))
		if result.JournalError != "" {
			t.Fatal(result.JournalError)
		}
		for _, item := range result.Items {
			if item.Result == api.ResultError {
				t.Fatal(item)
			}
		}
		var z public.DNSZone
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "migrate"}, &z); err != nil {
			t.Fatal(err)
		}
		if z.Spec.SOA.MName != preview.Zones[0].TargetNameservers[0] || z.Spec.Nameservers[0] != z.Spec.SOA.MName {
			t.Fatal("authority not committed", z.Spec)
		}
		if rs := preview.Zones[0].AddressRecordSet; rs != nil {
			var got public.DNSRecordSet
			if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(rs), &got); err != nil || len(got.Spec.Endpoints) != 2 {
				t.Fatal("VIP records missing", err, got.Spec)
			}
		}
		migrationServingDNS(t, f, "migrate.test.", dns.TypeNS, preview.Zones[0].TargetNameservers[0])
		migrationServingDNS(t, f, "migrate.test.", dns.TypeSOA, preview.Zones[0].TargetNameservers[0])
		if rs := preview.Zones[0].AddressRecordSet; rs != nil {
			for _, ep := range rs.Spec.Endpoints {
				migrationServingDNS(t, f, ep.DNSName, dns.StringToType[ep.RecordType], ep.Targets[0])
			}
		}
		journal := migrationDecode[api.MigrationRun](t, migrationServingRequest(t, f, s, http.MethodGet, path, nil, http.StatusOK))
		if journal.Data.Cutovers[len(journal.Data.Cutovers)-1].Status != "completed" {
			t.Fatal("incomplete cutover", journal)
		}
		migrationServingRequest(t, f, s, http.MethodPost, path+"/decommission", preview, http.StatusPreconditionFailed)
	}
}

func TestServingMigrationBackgroundSelectedDurablePartial(t *testing.T) {
	f := newServingCommandFixture(t)
	blocked := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "default"}, Spec: public.BlockListSpec{Mode: public.ListMode("block"), Sources: []public.ListSource{{Name: "file", Entries: []public.ListEntry{{Value: "example.test", Type: public.EntryExact}}}}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), blocked, store.ApplyOptions{Manager: store.ManagerFile("lists.yaml")}); err != nil {
		t.Fatal(err)
	}
	s := migrationServingAPI(t, f, nil)
	p := migrationServingPlan("good", "skip")
	p.Objects = append(p.Objects, map[string]any{"kind": "BlockList", "metadata": map[string]any{"name": "managed", "namespace": "default"}, "spec": map[string]any{"mode": "block", "sources": []any{map[string]any{"name": "migrated", "entries": []any{map[string]any{"value": "example.test", "type": "exact"}}}}}})
	p.Settings = map[string]any{"blocking": map[string]any{"enabled": false}}
	p.Selection = &importers.Selection{Objects: []string{"DNSZone/good", "BlockList/default/managed"}, Settings: []string{"blocking"}}
	started := migrationDecode[api.MigrationRun](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/start", p, http.StatusAccepted))
	var journal api.MigrationRun
	until := time.Now().Add(10 * time.Second)
	for {
		journal = migrationDecode[api.MigrationRun](t, migrationServingRequest(t, f, s, http.MethodGet, "/api/v1/imports/runs/"+started.ID, nil, http.StatusOK))
		if journal.Data.Status != "running" {
			break
		}
		if time.Now().After(until) {
			t.Fatal("job did not finish", journal)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if journal.Data.Status != "partial" {
		t.Fatal(journal.Data.Status)
	}
	for i, want := range []string{api.ResultCreated, "skipped", api.ResultError, "not_applied"} {
		if journal.Data.Items[i].Result != want {
			t.Fatal(i, journal.Data.Items)
		}
	}
	if journal.Data.Items[2].ErrorKey != api.KeyManagedBy || journal.Data.Items[0].ResourceVersion == "" {
		t.Fatal("lost durable report details", journal)
	}
	var skipped public.DNSZone
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "skip"}, &skipped), store.ErrNotFound) {
		t.Fatal("unselected zone imported")
	}
}

func TestServingMigrationOriginalProofRevocationStopsSubsequentStep(t *testing.T) {
	f := newServingCommandFixture(t)
	id, ok := tokens.Parse(f.token)
	if !ok {
		t.Fatal("token")
	}
	var attempts atomic.Int32
	command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
		next, err := f.s.MigrationCommand(ctx, p)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error {
			n := attempts.Add(1)
			if n == 3 {
				var token internal.APIToken
				if err := f.s.opts.Store.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
					return err
				}
				token.Spec.Scopes = []string{"viewer"}
				if _, err := f.s.opts.Store.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
					return err
				}
			}
			return next(ctx)
		}, nil
	}
	s := migrationServingAPI(t, f, command)
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", migrationServingPlan("first", "revoked"), http.StatusOK))
	if result.JournalError == "" || len(result.Items) != 1 || attempts.Load() != 3 {
		t.Fatal("revoked work continued", result, attempts.Load())
	}
	var r internal.MigrationRun
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: result.RunID}, &r); err != nil || r.Spec.Items[0].Result != api.ResultCreated || r.Spec.Items[1].Result != "pending" {
		t.Fatal("prior atomic success lost", err, r.Spec)
	}
	var z public.DNSZone
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "revoked"}, &z), store.ErrNotFound) {
		t.Fatal("revoked credential wrote future zone")
	}
}

func TestServingMigrationInitialFinalExpiryRollsBackJournal(t *testing.T) {
	f := newServingCommandFixture(t)
	migrationShortCredential(t, f)
	command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
		next, err := f.s.MigrationCommand(ctx, p)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error {
			var calls atomic.Int32
			f.s.opts.Now = func() time.Time {
				if calls.Add(1) <= 3 {
					return f.now
				}
				return f.expires
			}
			return next(ctx)
		}, nil
	}
	s := migrationServingAPI(t, f, command)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w := servingZoneRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", migrationServingPlan("expires"))
	if w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code, w.Body)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("final expiry committed journal", err)
	}
}

func TestServingMigrationStepRollbackIncludesVIPRecordsAndNoOp(t *testing.T) {
	for _, noOp := range []bool{false, true} {
		t.Run(map[bool]string{false: "VIP-write", true: "unchanged-zone"}[noOp], func(t *testing.T) {
			f := newServingCommandFixture(t)
			migrationShortCredential(t, f)
			normal := migrationServingAPI(t, f, nil)
			result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, normal, http.MethodPost, "/api/v1/imports/apply", migrationServingPlan("rollback"), http.StatusOK))
			path := "/api/v1/imports/runs/" + result.RunID
			target := map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}
			if noOp {
				target = map[string]any{"nameservers": []string{"old.test."}}
			}
			preview := migrationDecode[api.MigrationCutoverPlan](t, migrationServingRequest(t, f, normal, http.MethodPost, path+"/decommission/preview", target, http.StatusOK))
			var before public.DNSZone
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "rollback"}, &before); err != nil {
				t.Fatal(err)
			}
			command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
				next, err := f.s.MigrationCommand(ctx, p)
				if err != nil {
					return nil, err
				}
				n := 0
				return func(ctx context.Context) error {
					n++
					if n == 2 {
						var calls atomic.Int32
						f.s.opts.Now = func() time.Time {
							if calls.Add(1) <= 3 {
								return f.now
							}
							return f.expires
						}
					}
					return next(ctx)
				}, nil
			}
			failing := migrationServingAPI(t, f, command)
			outcome := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, failing, http.MethodPost, path+"/decommission", preview, http.StatusOK))
			if outcome.JournalError == "" || len(outcome.Items) != 0 {
				t.Fatal("failed step returned uncommitted results", outcome)
			}
			var after public.DNSZone
			if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&before), &after); err != nil || after.ResourceVersion != before.ResourceVersion {
				t.Fatal("failed final check changed authority", err, after.Spec)
			}
			if rs := preview.Zones[0].AddressRecordSet; rs != nil {
				var absent public.DNSRecordSet
				if !errors.Is(f.s.opts.Store.Get(t.Context(), store.KeyOf(rs), &absent), store.ErrNotFound) {
					t.Fatal("failed zone command committed VIP records")
				}
			}
			var run internal.MigrationRun
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: result.RunID}, &run); err != nil || len(run.Spec.Cutovers) != 1 || len(run.Spec.Cutovers[0].Items) != 0 {
				t.Fatal("failed step journaled success", err, run.Spec)
			}
		})
	}
}

func TestServingMigrationShutdownStopsDetachedOriginalPlan(t *testing.T) {
	f := newServingCommandFixture(t)
	entered := make(chan struct{})
	command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
		next, err := f.s.MigrationCommand(ctx, p)
		if err != nil {
			return nil, err
		}
		n := 0
		return func(ctx context.Context) error {
			n++
			if n == 3 {
				close(entered)
				<-ctx.Done()
			}
			return next(ctx)
		}, nil
	}
	s := migrationServingAPI(t, f, command)
	start := migrationDecode[api.MigrationRun](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/start", migrationServingPlan("kept", "canceled"), http.StatusAccepted))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached second object")
	}
	s.Close()
	var journal internal.MigrationRun
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: start.ID}, &journal); err != nil || journal.Spec.Items[0].Result != api.ResultCreated || journal.Spec.Items[1].Result != "pending" {
		t.Fatal("shutdown lost durable progress", err, journal.Spec)
	}
	var canceled public.DNSZone
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "canceled"}, &canceled), store.ErrNotFound) {
		t.Fatal("shutdown allowed pending work")
	}
}

func TestServingMigrationHistoryRetainsOriginalCompleteImage(t *testing.T) {
	f := newServingCommandFixture(t)
	s := migrationServingAPI(t, f, nil)
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", migrationServingPlan("history"), http.StatusOK))
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	reads := servingbuild.NewMigrationReads()
	var held context.Context
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		held = r.Context()
		before, base, err := reads.MigrationSnapshot(held)
		if err != nil || len(before) != 1 {
			t.Fatal("initial history", err, len(before))
		}
		before[0].Spec.Failure = "local caller mutation"
		var journal internal.MigrationRun
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: result.RunID}, &journal); err != nil {
			t.Fatal(err)
		}
		journal.Spec.Failure = "new durable history"
		if _, err := f.s.opts.Store.Apply(t.Context(), &journal, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		again, same, err := reads.MigrationSnapshot(held)
		if err != nil || !base.Same(same) || len(again) != 1 || again[0].Spec.Failure != "" {
			t.Fatal("held history sampled caller/live replacement", err, again)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if _, _, err := reads.MigrationSnapshot(held); err == nil {
		t.Fatal("released history remained readable")
	}
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		current, _, err := reads.MigrationSnapshot(r.Context())
		if err != nil || len(current) != 1 || current[0].Spec.Failure != "new durable history" {
			t.Fatal("fresh history did not see replacement", err, current)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
}

func TestServingMigrationOriginalWriterStopsPendingWork(t *testing.T) {
	f := newServingCommandFixture(t)
	command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
		next, err := f.s.MigrationCommand(ctx, p)
		if err != nil {
			return nil, err
		}
		n := 0
		return func(ctx context.Context) error {
			n++
			if n == 2 {
				f.stop()
			}
			return next(ctx)
		}, nil
	}
	s := migrationServingAPI(t, f, command)
	result := migrationDecode[api.MigrationResult](t, migrationServingRequest(t, f, s, http.MethodPost, "/api/v1/imports/apply", migrationServingPlan("fenced"), http.StatusOK))
	if result.JournalError == "" || len(result.Items) != 0 {
		t.Fatal("writer replacement executed pending work", result)
	}
	var z public.DNSZone
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "fenced"}, &z), store.ErrNotFound) {
		t.Fatal("fenced writer committed destination")
	}
}

func migrationServingDNS(t *testing.T, f *servingCommandFixture, name string, qtype uint16, want string) {
	t.Helper()
	a := authoritative.New(nil)
	a.Capture = func(req *pipeline.Request) zone.Set {
		owner, err := servingdns.Component[*servingzones.Prepared](req, "zones")
		if err != nil {
			return nil
		}
		return owner.Zones(time.Now())
	}
	chain, err := pipeline.New(a)
	if err != nil {
		t.Fatal(err)
	}
	chain.SetCapture(servingdns.Capture(f.s.publisher))
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		return value.(*servingbuild.RuntimeOwner).Primary().Admitted("migrate.test.", time.Now())
	})
	msg := new(dns.Msg)
	msg.SetQuestion(name, qtype)
	response, err := chain.Serve(&pipeline.Request{Ctx: t.Context(), Msg: msg, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:5300"), Proto: pipeline.ProtoUDP}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Close()
	if response.Msg == nil || !response.Msg.Authoritative || !strings.Contains(response.Msg.String(), want) {
		t.Fatalf("committed migration not served: %s %s want %s: %v", name, dns.TypeToString[qtype], want, response.Msg)
	}
}

func migrationShortCredential(t *testing.T, f *servingCommandFixture) {
	t.Helper()
	id, ok := tokens.Parse(f.token)
	if !ok {
		t.Fatal("fixture token")
	}
	var token internal.APIToken
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
		t.Fatal(err)
	}
	f.expires = f.now.Add(time.Minute)
	expiry := metav1.NewTime(f.expires)
	token.Spec.ExpiresAt = &expiry
	if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
}
