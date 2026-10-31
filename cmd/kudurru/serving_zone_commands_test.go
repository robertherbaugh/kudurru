package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingZoneAPI(t *testing.T, f *servingCommandFixture, command api.ZoneCommand) *api.Server {
	t.Helper()
	if command == nil {
		command = f.s.ZoneCommand
	}
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		ObjectCommand: f.s.ObjectCommand, ZoneCommand: command, Authenticate: f.http.Middleware, Authorizer: f.http,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func servingZoneRequest(t *testing.T, f *servingCommandFixture, srv *api.Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	contentType := "application/json"
	if v, ok := body.(string); ok {
		raw, contentType = []byte(v), "text/dns"
	} else if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "http://kudurru.test"+path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func servingZoneFile(count int) string {
	var b strings.Builder
	b.WriteString("$ORIGIN compound.test.\n$TTL 60\n@ IN SOA ns hm 17 7200 3600 1209600 300\n@ IN NS ns\n")
	for i := range count {
		fmt.Fprintf(&b, "h%d IN A 192.0.2.%d\n", i, i%250+1)
	}
	return b.String()
}

func servingZoneReadyRequest(t *testing.T, f *servingCommandFixture, srv *api.Server, method, path string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	// The multi-chunk import makes complete-image validation expensive under
	// the race detector, and publication may span several bounded driver
	// attempts. This observation budget never extends an operation's deadline.
	start := time.Now()
	until := start.Add(2 * time.Minute)
	nextProgress := start.Add(10 * time.Second)
	attempts := 0
	for {
		for !f.s.publisher.Status().ProtectedReady {
			now := time.Now()
			if !now.Before(until) {
				t.Fatalf("%s %s did not converge after %s: publisher=%+v driver=%+v", method, path, time.Since(start), f.s.publisher.Status(), f.s.primary.driver.Status())
			}
			if !now.Before(nextProgress) {
				status := f.s.publisher.Status()
				t.Logf("awaiting %s %s after %s: installed=%d served=%d publication=%d preparing=%v reason=%s", method, path, now.Sub(start), status.Installed.Revision, status.Served.Base.Revision, status.Served.Publication, status.Preparing, status.Reason)
				nextProgress = now.Add(10 * time.Second)
			}
			time.Sleep(10 * time.Millisecond)
		}
		attempts++
		w := servingZoneRequest(t, f, srv, method, path, body)
		if w.Code == http.StatusServiceUnavailable && time.Now().Before(until) {
			// Retry using a distinct HTTP request. The command itself never
			// resamples the rejected request's original lease or writer.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if w.Code != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, want, w.Body)
		}
		status := f.s.publisher.Status()
		t.Logf("%s %s observed after %s (%d requests): installed=%d served=%d", method, path, time.Since(start), attempts, status.Installed.Revision, status.Served.Base.Revision)
		return w
	}
}

func TestServingZoneCommandActualHTTPCompleteSQLite(t *testing.T) {
	f := newServingCommandFixture(t)
	srv := servingZoneAPI(t, f, nil)
	request := func(method, path string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		return servingZoneReadyRequest(t, f, srv, method, path, body, want)
	}
	request(http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1500), http.StatusOK)
	for i, count := range []int{1000, 500} {
		var rs public.DNSRecordSet
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: fmt.Sprintf("import-compound-%d", i+1)}, &rs); err != nil || len(rs.Spec.Endpoints) != count {
			t.Fatal("incomplete durable import", err)
		}
	}
	ep := public.Endpoint{DNSName: "www", RecordType: "A", Targets: []string{"192.0.2.80"}, RecordTTL: 60}
	request(http.MethodPost, "/api/v1/zones/compound/records", ep, http.StatusCreated)
	ep.DNSName = "sibling"
	request(http.MethodPost, "/api/v1/zones/compound/records", ep, http.StatusCreated)
	request(http.MethodPost, "/api/v1/zones/compound/records", ep, http.StatusOK)
	request(http.MethodDelete, "/api/v1/zones/compound/records/www/A", nil, http.StatusNoContent)
	var current public.DNSRecordSet
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "api-compound"}, &current); err != nil || len(current.Spec.Endpoints) != 1 || current.Spec.Endpoints[0].DNSName != "sibling.compound.test" {
		t.Fatal("record merge/delete lost current sibling", err)
	}
	request(http.MethodDelete, "/api/v1/zones/compound?dryRun=true", nil, http.StatusOK)
	request(http.MethodPost, "/api/v1/zones/compound/import", servingZoneFile(1), http.StatusOK)
	var stale public.DNSRecordSet
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "import-compound-2"}, &stale), store.ErrNotFound) {
		t.Fatal("reimport did not remove stale chunk")
	}
	request(http.MethodDelete, "/api/v1/zones/compound", nil, http.StatusOK)
	var gone public.DNSZone
	if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "compound"}, &gone), store.ErrNotFound) {
		t.Fatal("cascade did not commit")
	}
}

func TestServingZoneCommandFinalExpiryIncludesEmptyAndDryRun(t *testing.T) {
	for _, mode := range []string{"import-write", "dry-run", "cascade-write", "missing-record", "missing-import"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			if mode == "dry-run" || mode == "cascade-write" {
				z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "compound"}, Spec: public.DNSZoneSpec{ZoneName: "compound.test.", Type: public.ZoneTypeForward}}
				if _, err := f.s.opts.Store.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				awaitServingZoneCommandForward(t, f, z.Name)
			}
			var clockCalls atomic.Int32
			f.s.opts.Now = func() time.Time {
				if clockCalls.Add(1) == 1 {
					return f.now
				}
				return f.expires
			}
			srv := servingZoneAPI(t, f, nil)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			method, path := http.MethodDelete, "/api/v1/zones/compound/records/absent/A"
			var body any
			switch mode {
			case "import-write":
				method, path, body = http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1500)
			case "dry-run":
				path = "/api/v1/zones/compound?dryRun=true"
			case "cascade-write":
				path = "/api/v1/zones/compound"
			case "missing-import":
				method, path, body = http.MethodPost, "/api/v1/zones/compound/import", servingZoneFile(0)
			}
			w := servingZoneRequest(t, f, srv, method, path, body)
			if w.Code != http.StatusUnauthorized || clockCalls.Load() < 2 {
				t.Fatal("operation escaped exact final credential expiry", w.Code, w.Body, clockCalls.Load())
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("final expiry changed durable complete image", err)
			}
		})
	}
}

func TestServingZoneCommandRejectsNilAndForeignPlans(t *testing.T) {
	f := newServingCommandFixture(t)
	for _, mode := range []string{"nil", "zero", "foreign-target", "released", "already-applied"} {
		t.Run(mode, func(t *testing.T) {
			called := false
			command := func(ctx context.Context, target api.ZoneCommandTarget, prepare api.ZoneCommandPrepare) error {
				if mode == "foreign-target" {
					target.Zone = "other"
				}
				return f.s.ZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.ZoneCommandPlan, error) {
					called = true
					if mode == "nil" {
						return nil, nil
					}
					if mode == "zero" {
						return new(api.ZoneCommandPlan), nil
					}
					plan, err := prepare(ctx, tx)
					if err == nil {
						switch mode {
						case "released":
							plan.Release()
						case "already-applied":
							if err := plan.Apply(ctx, tx); err != nil {
								return nil, err
							}
						}
					}
					return plan, err
				})
			}
			srv := servingZoneAPI(t, f, command)
			before, _ := f.s.opts.Store.ReadGeneration(t.Context())
			w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1))
			if !called || w.Code == http.StatusOK {
				t.Fatal("missing/foreign/reused admission accepted", mode, w.Code, w.Body)
			}
			after, _ := f.s.opts.Store.ReadGeneration(t.Context())
			if !before.Generation().Same(after.Generation()) {
				t.Fatal("rejected plan committed prepared changes", mode)
			}
		})
	}
	called := false
	if err := f.s.ZoneCommand(t.Context(), api.ZoneCommandTarget{Zone: "compound", Action: "unknown"}, func(context.Context, *sqlite.CommandTx) (*api.ZoneCommandPlan, error) { called = true; return nil, nil }); err == nil || called {
		t.Fatal("unknown action entered callback")
	}
}

func TestServingZoneCommandExactRequirements(t *testing.T) {
	target := api.ZoneCommandTarget{Zone: "compound", Action: api.ZoneCommandImport}
	base := rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: target.Zone}, Name: target.Zone}
	valid := []rbac.Attributes{base, {Resource: "zones", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Name: target.Zone}, {Resource: "records", Verb: "delete", Scope: base.Scope, Name: target.Zone}}
	if !zoneCommandRequirements(target, valid) {
		t.Fatal("complete import requirements rejected")
	}
	for _, mode := range []string{"missing", "duplicate", "foreign-zone", "private", "namespace"} {
		attrs := append([]rbac.Attributes(nil), valid...)
		switch mode {
		case "missing":
			attrs = attrs[1:]
		case "duplicate":
			attrs[1] = attrs[0]
		case "foreign-zone":
			attrs[0].Scope.Name = "foreign"
		case "private":
			attrs[0].Resource = "tokens"
		case "namespace":
			attrs[0].Namespace = "foreign"
		}
		if zoneCommandRequirements(target, attrs) {
			t.Fatal("malformed requirements accepted", mode)
		}
	}
	target.Action = api.ZoneCommandDeleteRecord
	if zoneCommandRequirements(target, []rbac.Attributes{base}) {
		t.Fatal("endpoint delete accepted a write-only decision")
	}
	base.Verb = "delete"
	if !zoneCommandRequirements(target, []rbac.Attributes{base}) || !reflect.DeepEqual(valid[0].Verb, "write") {
		t.Fatal("removal-only permission contract changed")
	}
}

func TestServingZoneCommandReauthenticatesCurrentRawProofAndAllScopes(t *testing.T) {
	for _, mode := range []string{"create-cluster", "missing-record", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			if mode == "dry-run" {
				z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "compound"}, Spec: public.DNSZoneSpec{ZoneName: "compound.test.", Type: public.ZoneTypeForward}}
				if _, err := f.s.opts.Store.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				awaitServingZoneCommandForward(t, f, z.Name)
			}
			id, ok := tokens.Parse(f.token)
			if !ok {
				t.Fatal("fixture token is invalid")
			}
			called := false
			command := func(ctx context.Context, target api.ZoneCommandTarget, prepare api.ZoneCommandPrepare) error {
				return f.s.ZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.ZoneCommandPlan, error) {
					plan, err := prepare(ctx, tx)
					if err != nil {
						return plan, err
					}
					// Route middleware saw the original admin proof. Replace its
					// current scope inside the actual transaction so only raw-proof
					// reauthentication can reject the sealed complete action.
					var token internalv1.APIToken
					if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
						return plan, err
					}
					token.Spec.Scopes = []string{"viewer"}
					if mode == "create-cluster" {
						// This grants records.write at the target, but cannot create
						// the absent DNSZone with cluster zones.write.
						token.Spec.Scopes = []string{"zone-admin@zone:compound"}
					}
					if _, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
						return plan, err
					}
					called = true
					return plan, nil
				})
			}
			srv := servingZoneAPI(t, f, command)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			method, path := http.MethodDelete, "/api/v1/zones/compound/records/absent/A"
			var body any
			switch mode {
			case "create-cluster":
				method, path, body = http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1500)
			case "dry-run":
				path = "/api/v1/zones/compound?dryRun=true"
			}
			w := servingZoneRequest(t, f, srv, method, path, body)
			if !called || w.Code != http.StatusForbidden {
				t.Fatal("cached admin authority bypassed current scopes", mode, w.Code, w.Body, called)
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("denied current scope changed durable image", err)
			}
		})
	}
}
