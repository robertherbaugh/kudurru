package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingDomainAPI(t *testing.T, f *servingCommandFixture, command api.DomainCommand) *api.Server {
	t.Helper()
	if command == nil {
		command = f.s.DomainCommand
	}
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, ObjectCommand: f.s.ObjectCommand, DomainCommand: command, Now: f.s.opts.Now, Authorizer: f.http, Authenticate: func(next http.Handler) http.Handler { return f.http.Middleware(servingMutationRoutes(next)) }})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestServingDomainCommandActualSQLite(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	if _, err := f.s.opts.Store.Apply(t.Context(), &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}}, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	srv := servingDomainAPI(t, f, nil)
	w := servingObjectRequest(t, f, srv, http.MethodPost, "/api/v1/allowlist", map[string]any{"values": []string{"Ads.Example.", "bad name"}}, "")
	if w.Code != http.StatusOK {
		t.Fatal("domain add failed", w.Code, w.Body)
	}
	var out api.DomainWriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Items) != 1 || out.Items[0].Value != "ads.example" || len(out.Processed.Errors) != 1 || len(out.Attached) != 1 {
		t.Fatal("bad domain receipt", err, out)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	before, _ := f.s.opts.Store.ReadGeneration(t.Context())
	w = servingObjectRequest(t, f, srv, http.MethodPost, "/api/v1/allowlist", map[string]any{"values": []string{"ads.example"}}, "")
	if w.Code != http.StatusOK {
		t.Fatal("domain dedup failed", w.Code, w.Body)
	}
	after, _ := f.s.opts.Store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("domain dedup mutated durable state")
	}
	w = servingObjectRequest(t, f, srv, http.MethodDelete, "/api/v1/allowlist/"+out.Items[0].ID, nil, "")
	if w.Code != http.StatusNoContent {
		t.Fatal("domain delete failed", w.Code, w.Body)
	}
}

func TestServingDomainCommandFinalExpiryAndWriterIncludesNoop(t *testing.T) {
	for _, mode := range []string{"write", "dedup", "invalid-values", "missing", "batch-errors", "writer"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			defer f.stop()
			if mode == "dedup" {
				if _, err := f.s.opts.Store.Apply(t.Context(), &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "ui-allow", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Value: "ads.example"}}}}}}, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
			}
			var clockCalls atomic.Int32
			command := f.s.DomainCommand
			if mode == "writer" {
				command = func(ctx context.Context, target api.DomainCommandTarget, prepare api.DomainCommandPrepare) error {
					return f.s.DomainCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.DomainCommandPlan, error) {
						plan, err := prepare(ctx, tx)
						f.cancel()
						return plan, err
					})
				}
			}
			srv := servingDomainAPI(t, f, command)
			// Keep the editor's creation clock stable; advance only root's
			// authentication clock between admission and final commit checks.
			if mode != "writer" {
				f.s.opts.Now = func() time.Time {
					if clockCalls.Add(1) == 1 {
						return f.now
					}
					return f.expires
				}
			}
			before, _ := f.s.opts.Store.ReadGeneration(t.Context())
			method, path, body := http.MethodPost, "/api/v1/allowlist", any(map[string]any{"values": []string{"ads.example"}})
			switch mode {
			case "invalid-values":
				body = map[string]any{"values": []string{"bad name"}}
			case "missing":
				method, path, body = http.MethodDelete, "/api/v1/allowlist/ZGVmYXVsdABleGFjdABtaXNzaW5nLmV4YW1wbGU", nil
			case "batch-errors":
				path, body = "/api/v1/allowlist:batch", map[string]any{"ids": []string{"bad"}, "op": "delete"}
			}
			w := servingObjectRequest(t, f, srv, method, path, body, "")
			if w.Code < 400 {
				t.Fatal("command escaped final admission", w.Code, w.Body)
			}
			if mode != "writer" && (w.Code != http.StatusUnauthorized || clockCalls.Load() < 2) {
				t.Fatal("command did not reach final expiry", w.Code, w.Body)
			}
			f.stop()
			after, _ := f.s.opts.Store.ReadGeneration(t.Context())
			if !before.Generation().Same(after.Generation()) {
				t.Fatal("refused command changed state")
			}
		})
	}
}

func TestServingDomainCommandRetainsOriginalLeaseAndPlan(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	for _, mode := range []string{"nil", "released", "foreign", "stale"} {
		t.Run(mode, func(t *testing.T) {
			entered := false
			command := func(ctx context.Context, target api.DomainCommandTarget, prepare api.DomainCommandPrepare) error {
				if mode == "stale" {
					if _, err := f.s.opts.Store.Apply(t.Context(), &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "changed", Namespace: "default"}}, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						t.Fatal(err)
					}
					awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
				}
				if mode == "foreign" {
					target.Operation = "patch"
				}
				return f.s.DomainCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.DomainCommandPlan, error) {
					entered = true
					if mode == "nil" {
						return nil, nil
					}
					plan, err := prepare(ctx, tx)
					if mode == "released" && plan != nil {
						plan.Release()
					}
					return plan, err
				})
			}
			srv := servingDomainAPI(t, f, command)
			w := servingObjectRequest(t, f, srv, http.MethodPost, "/api/v1/allowlist", map[string]any{"values": []string{"ads.example"}}, "")
			if w.Code < 400 || mode == "stale" && entered {
				t.Fatal("repaired original lease or accepted foreign plan", w.Code, entered)
			}
			var list public.BlockList
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "ui-allow"}, &list); err == nil {
				t.Fatal("failed plan changed objects")
			}
		})
	}
}

func TestServingDomainCommandReauthenticatesOriginalRawProof(t *testing.T) {
	for _, mode := range []string{"write", "dedup", "batch-errors"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			defer f.stop()
			if mode == "dedup" {
				if _, err := f.s.opts.Store.Apply(t.Context(), &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "ui-allow", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Value: "ads.example"}}}}}}, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
			}
			id, _ := tokens.Parse(f.token)
			called := false
			command := func(ctx context.Context, target api.DomainCommandTarget, prepare api.DomainCommandPrepare) error {
				return f.s.DomainCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.DomainCommandPlan, error) {
					plan, err := prepare(ctx, tx)
					if err != nil {
						return plan, err
					}
					// Middleware saw admin. Only current raw-proof reauthentication can
					// detect this transaction-local reduction of the actual token scopes.
					var token internalv1.APIToken
					if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
						return plan, err
					}
					token.Spec.Scopes = []string{"viewer"}
					if _, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
						return plan, err
					}
					called = true
					return plan, nil
				})
			}
			srv := servingDomainAPI(t, f, command)
			before, _ := f.s.opts.Store.ReadGeneration(t.Context())
			path, body := "/api/v1/allowlist", any(map[string]any{"values": []string{"ads.example"}})
			if mode == "batch-errors" {
				path, body = "/api/v1/allowlist:batch", map[string]any{"ids": []string{"bad"}, "op": "delete"}
			}
			w := servingObjectRequest(t, f, srv, http.MethodPost, path, body, "")
			if w.Code != http.StatusForbidden || !called {
				t.Fatal("used captured principal instead of original raw proof", w.Code, w.Body)
			}
			after, _ := f.s.opts.Store.ReadGeneration(t.Context())
			if !before.Generation().Same(after.Generation()) {
				t.Fatal("failed current authorization changed durable image")
			}
		})
	}
}
