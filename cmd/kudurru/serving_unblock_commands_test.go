package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	private "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/unblock"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingUnblockAPI(t *testing.T, f *brandingHTTPFixture, command unblock.Command) *api.Server {
	t.Helper()
	if command == nil {
		command = f.s.UnblockCommand
	}
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, ObjectCommand: f.s.ObjectCommand, UnblockCommand: command, Authenticate: f.adapter.Middleware, Authorizer: f.adapter, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}
func servingUnblockSeed(t *testing.T, f *brandingHTTPFixture) *public.UnblockRequest {
	t.Helper()
	svc := unblock.New(unblock.Options{Store: f.s.opts.Store, Now: f.now})
	r, err := svc.Submit(t.Context(), unblock.Submission{Client: netip.MustParseAddr("192.0.2.1"), Domain: "requested.example"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.opts.Store.Apply(t.Context(), &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}}, store.ApplyOptions{Manager: store.ManagerBootstrap})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	return r
}
func servingUnblockHTTP(t *testing.T, f *brandingHTTPFixture, srv *api.Server, action string, body any, want int) {
	t.Helper()
	cf := &servingCommandFixture{s: f.s, http: f.adapter, token: f.token}
	servingZoneReadyRequest(t, cf, srv, http.MethodPost, "/api/v1/unblockrequests/default/"+unblock.Name(nil, "192.0.2.1", "requested.example")+"/"+action, body, want)
}
func TestServingUnblockCurrentSQLiteAndDelegatedPermission(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	r := servingUnblockSeed(t, f)
	id, _ := tokens.Parse(f.token)
	var token private.APIToken
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
		t.Fatal(err)
	}
	token.Spec.Scopes = []string{"operator"}
	if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	command := func(ctx context.Context, target unblock.Target, d unblock.Decision) (*unblock.Outcome, error) {
		proof, err := servinghttp.Credential(ctx)
		if err != nil {
			return nil, err
		}
		var out *unblock.Outcome
		err = f.s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			// Restrict the otherwise normal operator to exactly the delegated grant.
			// A newly allocated catalog avoids mutating any pinned host dependency.
			raw, err := json.Marshal(deps.Permissions)
			if err != nil {
				return err
			}
			var restricted rbac.Permissions
			if err := json.Unmarshal(raw, &restricted); err != nil {
				return err
			}
			restricted.Roles["operator"] = []string{"unblockrequests.decide"}
			deps.Permissions = &restricted
			attrs := rbac.Attributes{Resource: "blocklists", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Namespace: "default", Name: "ui-allow"}
			if _, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, tx, deps, proof, []rbac.Attributes{attrs}, f.now); !errors.Is(err, servingauth.ErrDenied) {
				t.Fatal("fixture accidentally grants general writes", err)
			}
			out, err = f.s.executeUnblockCurrent(ctx, tx, deps, proof, target, d)
			return err
		})
		if err == nil {
			f.s.publisher.Wake()
		}
		return out, currentCommandAPIError(err)
	}
	srv := servingUnblockAPI(t, f, command)
	servingUnblockHTTP(t, f, srv, "approve", api.UnblockApproval{Duration: "1h"}, http.StatusOK)
	var current public.UnblockRequest
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(r), &current); err != nil || !current.Status.Allow.Added || current.Status.DecidedBy != "User:branding-admin" {
		t.Fatal("delegated command did not commit current decision", err)
	}
	var list public.BlockList
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "ui-allow"}, &list); err != nil || len(list.Spec.Sources[0].Entries) != 1 {
		t.Fatal("derived list effect absent", err)
	}
	servingUnblockHTTP(t, f, srv, "deny", api.UnblockDenial{Note: "revoked"}, http.StatusOK)
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "ui-allow"}, &list); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("revocation did not remove owned entry", err)
	}
}
func TestServingUnblockFinalCredentialAndEntitlement(t *testing.T) {
	for _, mode := range []string{"credential", "license", "license-clock-rollback", "license-removed"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrandingHTTPFixture(t)
			servingUnblockSeed(t, f)
			if mode == "license-removed" {
				if err := f.s.opts.License.Set("", "test"); err != nil {
					t.Fatal(err)
				}
				if err := f.s.host.ObserveLicense(t.Context()); err != nil {
					t.Fatal(err)
				}
				f.wait(t)
			}
			var calls atomic.Int32
			f.s.opts.Now = func() time.Time {
				var pcs [32]uintptr
				frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs[:])])
				for {
					frame, more := frames.Next()
					selected := strings.Contains(frame.Function, "executeUnblockCurrent.func1")
					if mode == "credential" {
						selected = strings.Contains(frame.Function, "ReauthorizeCurrentAllWithClock")
					}
					if selected && calls.Add(1) >= 2 {
						if mode == "credential" {
							return f.base.Add(90 * 24 * time.Hour)
						}
						if mode == "license-clock-rollback" {
							return f.base.Add(-time.Second)
						}
						return f.base.Add(time.Hour + license.Grace)
					}
					if !more {
						break
					}
				}
				return f.now()
			}
			before, _ := f.s.opts.Store.ReadGeneration(t.Context())
			srv := servingUnblockAPI(t, f, nil)
			cf := &servingCommandFixture{s: f.s, http: f.adapter, token: f.token}
			requestPath := "/api/v1/unblockrequests/default/" + unblock.Name(nil, "192.0.2.1", "requested.example") + "/approve"
			if mode == "license-removed" {
				servingZoneReadyRequest(t, cf, srv, http.MethodPost, requestPath, api.UnblockApproval{Duration: "1h"}, http.StatusPaymentRequired)
				after, _ := f.s.opts.Store.ReadGeneration(t.Context())
				if !before.Generation().Same(after.Generation()) {
					t.Fatal("license removal changed state")
				}
				return
			}
			w := servingZoneRequest(t, cf, srv, http.MethodPost, "/api/v1/unblockrequests/default/"+unblock.Name(nil, "192.0.2.1", "requested.example")+"/approve", api.UnblockApproval{Duration: "1h"})
			want := http.StatusPaymentRequired
			if mode == "credential" {
				want = http.StatusUnauthorized
			}
			if w.Code != want {
				t.Fatal("final current proof/entitlement escaped", w.Code, w.Body)
			}
			after, _ := f.s.opts.Store.ReadGeneration(t.Context())
			if !before.Generation().Same(after.Generation()) {
				t.Fatal("expired authority left partial effects")
			}
		})
	}
}
func TestServingUnblockCurrentRawProofRollback(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	servingUnblockSeed(t, f)
	command := func(ctx context.Context, target unblock.Target, d unblock.Decision) (*unblock.Outcome, error) {
		proof, err := servinghttp.Credential(ctx)
		if err != nil {
			return nil, err
		}
		var out *unblock.Outcome
		err = f.s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			id, _ := tokens.Parse(f.token)
			var token private.APIToken
			if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
				return err
			}
			token.Spec.Scopes = []string{"viewer"}
			if _, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			out, err = f.s.executeUnblockCurrent(ctx, tx, deps, proof, target, d)
			return err
		})
		return out, currentCommandAPIError(err)
	}
	before, _ := f.s.opts.Store.ReadGeneration(t.Context())
	servingUnblockHTTP(t, f, servingUnblockAPI(t, f, command), "approve", api.UnblockApproval{}, http.StatusForbidden)
	after, _ := f.s.opts.Store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("revoked current authority committed")
	}
}
func TestServingUnblockSweepUsesCanonicalPolicyAndOriginalWriter(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	r := servingUnblockSeed(t, f)
	// A decided request older than the host's default 30-day retention must be
	// retained when the current canonical document extends that retention.
	r.Status = public.UnblockRequestStatus{State: public.UnblockDenied, DecidedAt: &metav1.Time{Time: f.base.Add(-40 * 24 * time.Hour)}}
	if err := f.s.opts.Store.UpdateStatus(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	var settings public.Kudurru
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Settings = map[string]apiextensions.JSON{"unblockRequests": {Raw: []byte(`{"retention":"1440h","pendingExpiry":"1h"}`)}}
	if _, err := f.s.opts.Store.Apply(t.Context(), &settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	res, err := f.s.SweepUnblock(t.Context())
	if err != nil || res.Pruned != 0 {
		t.Fatal("stale host retention pruned current request", res, err)
	}
	settings.Spec.Settings["unblockRequests"] = apiextensions.JSON{Raw: []byte(`{"retention":"24h","pendingExpiry":"1h"}`)}
	if _, err := f.s.opts.Store.Apply(t.Context(), &settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	res, err = f.s.SweepUnblock(t.Context())
	if err != nil || res.Pruned != 1 {
		t.Fatal("current retention was not applied", res, err)
	}
	f.stop()
	if _, err := f.s.SweepUnblock(t.Context()); err == nil {
		t.Fatal("ended original writer admitted an empty sweep")
	}
}
