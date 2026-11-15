package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestServingSettingsRawDnstapReauthorizesQuerylogRaw drives the complete
// SQLite runtime's settings writer with an original credential that holds
// settings.write but not querylog.raw (D372). The API gate is bypassed on
// purpose: only the commit-time re-authorization (currentCommandAlso) stands
// between that credential and a raw dnstap sink, or an exclusion change
// under one. Each refusal leaves the durable generation untouched; the same
// credential still writes a sink that is not raw, and the administrator's
// credential writes both.
func TestServingSettingsRawDnstapReauthorizesQuerylogRaw(t *testing.T) {
	// The built-in table has no role with settings.write but without
	// querylog.raw; a custom role is what the separate permission is for.
	previous := loadProviderPermissions
	loadProviderPermissions = func() (*rbac.Permissions, error) {
		p, err := previous()
		if err != nil {
			return nil, err
		}
		p.Roles["settings-editor"] = []string{"settings.read", "settings.write"}
		return p, nil
	}
	t.Cleanup(func() { loadProviderPermissions = previous })
	f := newServingCommandFixture(t)
	editor, id := tokens.Generate()
	if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		expires := metav1.NewTime(f.expires)
		_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(f.now.Add(-time.Minute))}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(editor), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:root-command", Provider: "local"}, Scopes: []string{"settings-editor"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })

	sink := func(raw bool) func(*config.Config) {
		return func(cfg *config.Config) {
			cfg.Querylog.Sinks = []config.QuerylogSink{{Name: "collector", Type: config.QuerylogSinkDnstap, Dnstap: &config.QuerylogDnstap{Target: "unix:/run/dnstap/dnstap.sock", Raw: raw}}}
		}
	}
	excludeClients := func(cfg *config.Config) { cfg.Querylog.Exclude.Clients = []string{"10.0.0.0/8"} }
	// write runs one settings write under its own authenticated request and
	// reports its error and whether the durable generation moved.
	write := func(token string, mutate func(*config.Config), changed ...string) (bool, error) {
		t.Helper()
		before, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var werr error
		called := false
		r := httptest.NewRequest(http.MethodPatch, "http://kudurru.test/api/v1/settings", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			cfg, err := f.s.Settings(r.Context())
			if err != nil {
				t.Fatal(err)
			}
			mutate(cfg)
			werr = f.s.WriteSettings(r.Context(), cfg, changed)
		})).ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Fatal("authenticated request did not enter")
		}
		after, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		return !before.Generation().Same(after.Generation()), werr
	}
	forbidden := func(step string, moved bool, err error) {
		t.Helper()
		var e *api.Error
		if !errors.As(err, &e) || e.Status != http.StatusForbidden || moved {
			t.Fatalf("%s: want a refusal at commit with nothing persisted, got %v (generation moved %v)", step, err, moved)
		}
	}
	accepted := func(step string, moved bool, err error) {
		t.Helper()
		if err != nil || !moved {
			t.Fatalf("%s: want a persisted write, got %v (generation moved %v)", step, err, moved)
		}
	}

	moved, err := write(editor, sink(true), config.QuerylogSinksKey)
	forbidden("raw sink without querylog.raw", moved, err)
	moved, err = write(editor, sink(false), config.QuerylogSinksKey)
	accepted("plain sink with settings.write", moved, err)
	moved, err = write(f.token, sink(true), config.QuerylogSinksKey)
	accepted("raw sink by the administrator", moved, err)
	moved, err = write(editor, excludeClients, "querylog.exclude.clients")
	forbidden("exclusion change under a raw sink without querylog.raw", moved, err)
	moved, err = write(f.token, excludeClients, "querylog.exclude.clients")
	accepted("exclusion change by the administrator", moved, err)

	var cfg *config.Config
	r := httptest.NewRequest(http.MethodGet, "http://kudurru.test/api/v1/settings", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cfg, err = f.s.Settings(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	if err != nil || cfg == nil {
		t.Fatal("settings read failed", err)
	}
	if raws := config.RawQuerylogSinks(cfg.Querylog); len(raws) != 1 || len(cfg.Querylog.Exclude.Clients) != 1 {
		t.Fatalf("durable settings after the writes: raw sinks %v, excluded clients %v", raws, cfg.Querylog.Exclude.Clients)
	}
}
