package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func servingCacheEntries() *cache.Cache {
	rc := cache.New(cache.Options{MaxEntries: 100})
	for _, scope := range []pipeline.CacheScope{{ID: [32]byte{1}, Epoch: 1}, {ID: [32]byte{2}, Epoch: 2}} {
		key := cache.MakeScopedKey(scope, "target.example.", 1, 1, false, false, netip.Prefix{})
		rc.Set(key, []byte("wire"), cache.Positive, time.Minute)
	}
	return rc
}

func TestServingCacheCommandActualAdmissionAndLocalOnlyEffects(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	rc := servingCacheEntries()
	command, err := newServingCacheCommand(f.s, rc)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		generation := rc.Generation()
		removed, err := command(r.Context(), "target.example.")
		if err != nil || removed != 2 || rc.Len() != 0 || rc.Generation() != generation+1 {
			t.Fatal("current command did not flush both scoped keys", removed, err)
		}
		removed, err = command(r.Context(), "")
		if err != nil || removed != 0 || rc.Generation() != generation+2 {
			t.Fatal("admitted no-op did not fence detached fills", removed, err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if !called || err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("cache command changed durable generation or missed HTTP capture", called, err)
	}
}

func TestServingCacheCommandWaitCancellationAndPostcommitExclusion(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	var hold atomic.Bool
	entered, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	defer unblock()
	rc := cache.New(cache.Options{MaxEntries: 100, Now: func() time.Time {
		if hold.CompareAndSwap(true, false) {
			// Hold only the cache's local fill lock, outside command/dependency
			// pins. The admitted invalidation must wait for this fill to finish.
			close(entered)
			<-resume
		}
		return time.Now()
	}})
	key := cache.MakeKey("target.example.", 1, 1, false, false, netip.Prefix{})
	hold.Store(true)
	filled := make(chan struct{})
	go func() {
		defer close(filled)
		rc.Set(key, []byte("wire"), cache.Positive, time.Minute)
	}()
	<-entered
	command, err := newServingCacheCommand(f.s, rc)
	if err != nil {
		t.Fatal(err)
	}
	verifying := make(chan struct{})
	var once sync.Once
	clock := f.s.opts.Now
	f.s.opts.Now = func() time.Time { once.Do(func() { close(verifying) }); return clock() }
	type outcome struct {
		removed int
		err     error
	}
	first := make(chan outcome, 1)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			removed, err := command(r.Context(), "")
			first <- outcome{removed, err}
		})).ServeHTTP(httptest.NewRecorder(), f.request())
	}()
	defer func() { unblock(); <-filled; <-requestDone }()
	select {
	case <-verifying:
	case <-time.After(5 * time.Second):
		t.Fatal("first cache request never entered current authentication")
	}
	// Authentication has entered the real SQLite transaction. A separate
	// transaction must finish while local invalidation is still blocked.
	other, cancelOther := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelOther()
	if err := f.s.opts.Store.Command(other, f.s.opts.Store.Authority().Term, func(context.Context, *sqlite.CommandTx) error { return nil }); err != nil {
		t.Fatal("cache invalidation retained SQLite exclusion", err)
	}
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
		defer cancel()
		removed, err := command(ctx, "")
		if removed != 0 || err == nil {
			t.Error("canceled waiter entered the active flush", removed, err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called || rc.Generation() != 0 {
		t.Fatal("waiting request invalidated cache or missed authentication")
	}
	unblock()
	select {
	case result := <-first:
		if result.err != nil || result.removed != 1 || rc.Len() != 0 {
			t.Fatal("admitted flush failed after local fill joined", result.removed, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted flush never finished")
	}
}

func TestServingCacheCommandRefusesOriginalAuthorityLoss(t *testing.T) {
	for _, mode := range []string{"expiry", "regression", "expiry-empty", "writer", "request", "keys", "revoked", "scope", "released"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			defer f.stop()
			rc := servingCacheEntries()
			if mode == "expiry-empty" {
				rc.Flush()
			}
			command, err := newServingCacheCommand(f.s, rc)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "scope" {
				id, _ := tokens.Parse(f.token)
				var token internalv1.APIToken
				if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
					t.Fatal(err)
				}
				token.Spec.Scopes = []string{"viewer"}
				if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
				awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
			}
			count, generation := rc.Len(), rc.Generation()
			var held context.Context
			called := false
			check := func(ctx context.Context) {
				t.Helper()
				removed, err := command(ctx, "target.example.")
				if err == nil || removed != 0 {
					t.Error("refused current proof invalidated cache", removed, err)
				}
			}
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called, held = true, r.Context()
				ctx := r.Context()
				originalClock := f.s.opts.Now
				defer func() { f.s.opts.Now = originalClock }()
				switch mode {
				case "expiry", "expiry-empty", "regression":
					var calls atomic.Int64
					f.s.opts.Now = func() time.Time {
						if calls.Add(1) > 1 {
							if mode == "regression" {
								return f.now.Add(-time.Nanosecond)
							}
							return f.expires
						}
						return f.now
					}
				case "writer":
					f.cancel()
				case "request":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				case "keys":
					if err := f.s.opts.Keyrings.Replace(ctx, f.s.opts.Keyrings.Capture().Keys()); err != nil {
						t.Fatal(err)
					}
				case "revoked":
					id, _ := tokens.Parse(f.token)
					if err := f.s.opts.Store.Delete(ctx, store.Key{Kind: "APIToken", Name: id}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
						t.Fatal(err)
					}
				case "released":
					return
				}
				check(ctx)
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if mode == "released" {
				check(held)
			}
			if !called || rc.Len() != count || rc.Generation() != generation {
				t.Fatal("refusal changed entries or invalidation generation", called, rc.Len(), rc.Generation())
			}
		})
	}
}

func TestServingCacheCommandFollowerAndMissingCapture(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	rc := servingCacheEntries()
	if command, err := newServingCacheCommand(nil, rc); command != nil || err == nil {
		t.Fatal("nil runtime accepted")
	}
	if command, err := newServingCacheCommand(f.s, nil); command != nil || err == nil {
		t.Fatal("nil cache accepted")
	}
	command, err := newServingCacheCommand(f.s, rc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command(t.Context(), "target.example."); err == nil {
		t.Fatal("missing original HTTP proof accepted")
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	follower, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "cache-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
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
	opts := f.s.opts
	opts.Store = follower
	runtime, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runServingFixture(t, runtime)
	awaitServingRuntime(t, runtime, func() bool { return runtime.publisher.Status().ProtectedReady })
	httpAuth, err := servinghttp.New(servinghttp.Options{Publisher: runtime.publisher, Now: opts.Now})
	if err != nil {
		t.Fatal(err)
	}
	command, err = newServingCacheCommand(runtime, rc)
	if err != nil {
		t.Fatal(err)
	}
	generation := rc.Generation()
	called := false
	httpAuth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		_, err := command(r.Context(), "")
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
			t.Error("follower cache command did not refuse503", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called || rc.Len() != 2 || rc.Generation() != generation {
		t.Fatal("follower changed local cache or missed authentication")
	}
}
