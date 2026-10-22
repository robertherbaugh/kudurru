package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type servingNodeFixture struct {
	f       *servingCommandFixture
	reload  *config.Reloadable
	level   *slog.LevelVar
	command api.NodeConfigCommand
	api     *api.Server
}

func newServingNodeFixture(t *testing.T) *servingNodeFixture {
	t.Helper()
	n := &servingNodeFixture{f: newServingCommandFixture(t), level: new(slog.LevelVar)}
	cfg, err := cloneServingNodeConfig(n.f.s.opts.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Log.Level, cfg.Log.Format = "info", "json"
	cfg.Source, cfg.SettingsFile = "startup-node.yaml", filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(cfg.SettingsFile, []byte("cache:\n  maxEntries: 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.reload = config.NewReloadable(cfg)
	n.command, err = newServingNodeCommand(n.f.s, n.reload, n.level)
	if err != nil {
		t.Fatal(err)
	}
	n.api, err = api.New(api.Options{Store: n.f.s.opts.Store, Validator: n.f.s.opts.Validator, Config: cfg, Reload: n.reload, CanonicalSettings: n.f.s, NodeConfigCommand: n.command, Authenticate: n.f.http.Middleware, Authorizer: n.f.http, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (n *servingNodeFixture) call(ctx context.Context, method, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://kudurru.test/api/v1/node/config", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+n.f.token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	n.api.ServeHTTP(w, r)
	return w
}

func servingNodeBody(t *testing.T, w *httptest.ResponseRecorder) api.SettingsBody {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("node request failed: %d %s", w.Code, w.Body)
	}
	var body api.SettingsBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestServingNodeCommandActualHTTPAdoptionAndDesiredReadback(t *testing.T) {
	n := newServingNodeFixture(t)
	defer n.f.stop()
	before, err := n.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := n.f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id := lease.Bundle().Identity()
	lease.Release()
	startup, err := cloneServingNodeConfig(n.f.s.opts.Config)
	if err != nil {
		t.Fatal(err)
	}
	path := n.reload.Current().SettingsFile
	file, err := os.ReadFile(path) //nolint:gosec // This test created the file in t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: n.level}))
	logger.Debug("before")
	if log.Len() != 0 {
		t.Fatal("fixture logger is not actually level controlled")
	}
	body := servingNodeBody(t, n.call(t.Context(), http.MethodPatch, `{"log":{"level":"debug"}}`))
	if n.level.Level() != slog.LevelDebug || n.reload.Current().Log.Level != "debug" || body.Persisted == nil || *body.Persisted {
		t.Fatal("success did not acknowledge actual local level", body)
	}
	logger.Debug("after")
	if !strings.Contains(log.String(), `"msg":"after"`) {
		t.Fatal("actual logger failed to adopt level", log.String())
	}
	body = servingNodeBody(t, n.call(t.Context(), http.MethodPatch, `{"log":{"format":"text"}}`))
	if !reflect.DeepEqual(body.RestartRequired, []string{"log.format"}) || *body.Persisted || n.level.Level() != slog.LevelDebug {
		t.Fatal("format pretended hot/durable adoption", body)
	}
	log.Reset()
	logger.Debug("still JSON")
	if !json.Valid(bytes.TrimSpace(log.Bytes())) {
		t.Fatal("desired format changed running handler")
	}
	body = servingNodeBody(t, n.call(t.Context(), http.MethodGet, ""))
	if body.Settings["log"].(map[string]any)["format"] != "text" || body.Settings["tsigKeys"] != api.Redacted {
		t.Fatal("local document readback/redaction lost", body)
	}
	// An identical desired value still goes through actual current admission.
	servingNodeBody(t, n.call(t.Context(), http.MethodPatch, `{"log":{"level":"debug"}}`))
	after, err := n.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("local command changed durable cluster generation", err)
	}
	lease, err = n.f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Bundle().Identity() != id {
		t.Fatal("local command replaced whole serving cut")
	}
	lease.Release()
	if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, file) || !reflect.DeepEqual(n.f.s.opts.Config, startup) { //nolint:gosec // Same test-created temporary file.
		t.Fatal("local command changed startup/file inputs", err)
	}
	// Restart reconstruction uses startup inputs, not the transient document.
	restarted := config.NewReloadable(startup)
	if restarted.Current().Log.Format == "text" || restarted.Current().Log.Level == "debug" {
		t.Fatal("in-memory requested values fabricated restart persistence")
	}
}

func TestServingNodeCommandActualConcurrentLeavesReceiptsAndCancellation(t *testing.T) {
	n := newServingNodeFixture(t)
	defer n.f.stop()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	// Explicit test-only postcommit barrier; production installs no subscribers.
	stop := n.reload.Subscribe(func(_, _ *config.Config, paths []string) {
		if len(paths) == 1 && paths[0] == "log.level" {
			close(entered)
			<-release
		}
	})
	defer stop()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- n.call(t.Context(), http.MethodPatch, `{"log":{"level":"debug"}}`) }()
	select {
	case <-entered:
	case w := <-first:
		t.Fatalf("first command missed postcommit barrier: %d %s", w.Code, w.Body)
	case <-time.After(5 * time.Second):
		t.Fatal("local install did not reach barrier")
	}
	// The actual no-op Command has returned: another SQLite command can run
	// while local receipt delivery is held, proving there is no tx/pin reentry.
	if err := n.f.s.opts.Store.Command(t.Context(), n.f.s.opts.Store.Authority().Term, func(context.Context, *sqlite.CommandTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- n.call(t.Context(), http.MethodPatch, `{"log":{"format":"text"}}`) }()
	called := false
	n.f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Millisecond)
		defer cancel()
		next := *n.reload.Current()
		next.Log.Format = "text"
		if _, err := n.command(ctx, &next, []string{"log.format"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Error("waiting local gate ignored cancellation", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), n.f.request())
	if !called {
		t.Fatal("waiting request failed native authentication")
	}
	finish()
	a := servingNodeBody(t, <-first)
	b := servingNodeBody(t, <-second)
	if a.Settings["log"].(map[string]any)["format"] != "json" || b.Settings["log"].(map[string]any)["format"] != "text" || b.Settings["log"].(map[string]any)["level"] != "debug" || n.level.Level() != slog.LevelDebug {
		t.Fatal("concurrent scalar commands lost local document", a, b)
	}
}

func TestServingNodeCommandActualIgnoresWholeNextAndDetachesReceipts(t *testing.T) {
	n := newServingNodeFixture(t)
	defer n.f.stop()
	called := false
	n.f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		next := *n.f.s.opts.Config // deliberately stale, unrelated values ignored
		next.Log.Level, next.Node.Name, next.Cache.MaxEntries = "warn", "forged-node", 1
		before := n.reload.Current()
		receipt, err := n.command(r.Context(), &next, []string{"log.level"})
		if err != nil {
			t.Fatal(err)
		}
		if receipt.After.Node.Name != before.Node.Name || receipt.After.Cache.MaxEntries != before.Cache.MaxEntries || receipt.After.Source != before.Source || receipt.After.SettingsFile != before.SettingsFile || n.level.Level() != slog.LevelWarn {
			t.Fatal("whole next replaced unrelated local fields")
		}
		receipt.Before.Listeners.DNS[0].Address = "mutated-before"
		receipt.After.Listeners.DNS[0].Address = "mutated-after"
		if n.reload.Current().Listeners.DNS[0].Address == "mutated-after" || before.Listeners.DNS[0].Address == "mutated-before" {
			t.Fatal("receipt aliases installed document")
		}
	})).ServeHTTP(httptest.NewRecorder(), n.f.request())
	if !called {
		t.Fatal("command handler unavailable")
	}
}

func TestServingNodeCommandRejectsPartialInputsAndLockedLeaves(t *testing.T) {
	n := newServingNodeFixture(t)
	defer n.f.stop()
	for _, mode := range []string{"runtime", "reload", "document", "level"} {
		runtime, reload, level := n.f.s, n.reload, n.level
		switch mode {
		case "runtime":
			runtime = nil
		case "reload":
			reload = nil
		case "document":
			reload = config.NewReloadable(nil)
		case "level":
			level = nil
		}
		if command, err := newServingNodeCommand(runtime, reload, level); err == nil || command != nil {
			t.Fatal("partial constructor accepted", mode)
		}
	}
	before := n.reload.Current()
	if _, err := n.command(t.Context(), before, []string{"log.level"}); err == nil {
		t.Fatal("missing original complete HTTP capture accepted")
	}
	called := false
	n.f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		for _, input := range []struct {
			paths []string
			level string
		}{
			{nil, "debug"},
			{[]string{"cache.maxEntries"}, "debug"},
			{[]string{"log.level", "log.level"}, "debug"},
			{[]string{"log.level", "log.format", "log.level"}, "debug"},
			{[]string{"log.level"}, strings.Repeat("a", 257)},
			{[]string{"log.level"}, "loud"},
		} {
			next := *before
			next.Log.Level = input.level
			_, err := n.command(r.Context(), &next, input.paths)
			var failure *api.Error
			if !errors.As(err, &failure) || failure.Status != http.StatusBadRequest {
				t.Error("malformed callback accepted", input.paths, err)
			}
		}
		locked, err := cloneServingNodeConfig(before)
		if err != nil {
			t.Fatal(err)
		}
		locked.EnvLocked = []string{"log.level"}
		n.reload.Set(locked)
		next := *before // the API's earlier document did not contain this lock
		next.Log.Level = "debug"
		_, err = n.command(r.Context(), &next, []string{"log.level"})
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Status != http.StatusConflict || n.reload.Current() != locked {
			t.Error("current environment lock was bypassed", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), n.f.request())
	if !called || n.level.Level() != slog.LevelInfo || before.Log.Level != "info" {
		t.Fatal("invalid callback altered local level", called)
	}
}

func TestServingNodeCommandActualRefusesStaleAuthorityAndFinalExpiry(t *testing.T) {
	for _, mode := range []string{"expired-final", "expired-final-noop", "canceled-writer", "canceled-request", "rotated-keys", "revoked-after-capture", "current-scope"} {
		t.Run(mode, func(t *testing.T) {
			n := newServingNodeFixture(t)
			defer n.f.stop()
			before := n.reload.Current()
			if mode == "current-scope" {
				if err := n.f.s.opts.Store.Command(t.Context(), n.f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					rows, err := tx.List(ctx, "APIToken", store.ListOptions{})
					if err != nil {
						return err
					}
					for _, row := range rows.Items {
						token := row.(*internalv1.APIToken)
						token.Spec.Scopes = []string{"operator"}
						if _, err := tx.Apply(ctx, token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				awaitServingRuntime(t, n.f.s, func() bool { return n.f.s.publisher.Status().ProtectedReady })
			}
			called := false
			n.f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				ctx := r.Context()
				originalClock := n.f.s.opts.Now
				defer func() { n.f.s.opts.Now = originalClock }()
				switch mode {
				case "expired-final", "expired-final-noop":
					var calls atomic.Int64
					n.f.s.opts.Now = func() time.Time {
						if calls.Add(1) >= 2 {
							return n.f.expires
						}
						return n.f.now
					}
				case "canceled-writer":
					n.f.cancel()
				case "canceled-request":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				case "rotated-keys":
					if err := n.f.s.opts.Keyrings.Replace(ctx, n.f.s.opts.Keyrings.Capture().Keys()); err != nil {
						t.Fatal(err)
					}
				case "revoked-after-capture":
					if err := n.f.s.opts.Store.Command(ctx, n.f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						var user internalv1.LocalUser
						if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
							return err
						}
						user.Spec.Disabled = true
						_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				next := *before
				if mode != "expired-final-noop" {
					next.Log.Level = "debug"
				}
				_, err := n.command(ctx, &next, []string{"log.level"})
				if err == nil {
					t.Error("refused authority adopted local settings")
				}
				var failure *api.Error
				if strings.HasPrefix(mode, "expired-final") && (!errors.As(err, &failure) || failure.Status != http.StatusUnauthorized) {
					t.Error("actual final credential clock was not checked", err)
				}
				if mode == "current-scope" && (!errors.As(err, &failure) || failure.Status != http.StatusForbidden) {
					t.Error("node/config was not checked against current raw scope", err)
				}
			})).ServeHTTP(httptest.NewRecorder(), n.f.request())
			if !called || n.reload.Current() != before || n.level.Level() != slog.LevelInfo {
				t.Fatal("refusal changed memory or missed captured request", called)
			}
		})
	}
}

func TestServingNodeCommandActualPostcommitCancellationIsTruthful(t *testing.T) {
	n := newServingNodeFixture(t)
	defer n.f.stop()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := n.reload.Subscribe(func(_, _ *config.Config, _ []string) { cancel() })
	defer stop()
	w := n.call(ctx, http.MethodPatch, `{"log":{"level":"debug"}}`)
	if w.Code == http.StatusOK || n.level.Level() != slog.LevelDebug || n.reload.Current().Log.Level != "debug" {
		t.Fatal("postcommit cancellation fabricated atomic rollback/success", w.Code, w.Body)
	}
	body := servingNodeBody(t, n.call(t.Context(), http.MethodGet, ""))
	if body.Settings["log"].(map[string]any)["level"] != "debug" {
		t.Fatal("GET hid adopted local effect")
	}
	stop()
	servingNodeBody(t, n.call(t.Context(), http.MethodPatch, `{"log":{"level":"debug"}}`))
}

func TestServingNodeCommandActualFollowerUnavailable(t *testing.T) {
	n := newServingNodeFixture(t)
	defer n.f.stop()
	image, err := n.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	follower, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: n.f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "node-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = follower.Close() })
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term}); err != nil {
		t.Fatal(err)
	}
	options := n.f.s.opts
	options.Store = follower
	runtime, err := newServingRuntime(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runServingFixture(t, runtime)
	awaitServingRuntime(t, runtime, func() bool { return runtime.publisher.Status().ProtectedReady })
	httpAuth, err := servinghttp.New(servinghttp.Options{Publisher: runtime.publisher, Now: options.Now})
	if err != nil {
		t.Fatal(err)
	}
	command, err := newServingNodeCommand(runtime, n.reload, n.level)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	httpAuth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		next := *n.reload.Current()
		next.Log.Level = "debug"
		_, err := command(r.Context(), &next, []string{"log.level"})
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
			t.Error("follower local write did not fail503", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), n.f.request())
	if !called || n.level.Level() != slog.LevelInfo || n.reload.Current().Log.Level != "info" {
		t.Fatal("follower changed local config or missed authenticated request", called)
	}
}
