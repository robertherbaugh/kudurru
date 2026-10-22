package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type servingCommandFixture struct {
	s       *servingRuntime
	http    *servinghttp.Adapter
	token   string
	now     time.Time
	expires time.Time
	offset  atomic.Int64
	cancel  context.CancelFunc
	stop    func()
}

func newServingCommandFixture(t *testing.T) *servingCommandFixture {
	t.Helper()
	f := &servingCommandFixture{s: newServingRuntimeFixture(t), now: time.Now().UTC().Truncate(time.Second)}
	f.expires = f.now.Add(time.Hour)
	var id string
	f.token, id = tokens.Generate()
	f.s.opts.Now = func() time.Time { return f.now.Add(time.Duration(f.offset.Load())) }
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		// This command fixture has no primary lifecycle work competing with the
		// object/revision assertions; the real driver/elector still run normally.
		if err := tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		created := metav1.NewTime(f.now.Add(-time.Minute))
		user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "root-command", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		expires := metav1.NewTime(f.expires)
		_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: created}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(f.token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:root-command", Provider: "local"}, Scopes: []string{"admin"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	runServingFixture(t, f.s)
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	done := make(chan error, 1)
	go func() {
		done <- f.s.opts.Store.Elector("http-command-test").Run(ctx, func(ctx context.Context) {
			if err := f.s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("HTTP command writer failed to join")
			}
		})
	}
	t.Cleanup(f.stop)
	awaitServingRuntime(t, f.s, func() bool { return f.s.primary.driver.Status().Ready && f.s.publisher.Status().ProtectedReady })
	f.http, err = servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.s.opts.Now})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

var commandSettingsWrite = rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}

func (f *servingCommandFixture) request() *http.Request {
	r := httptest.NewRequest(http.MethodPatch, "http://kudurru.test/api/v1/settings", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	return r
}

func stageCommandLabel(ctx context.Context, tx *sqlite.CommandTx, _ auth.Principal) error {
	var settings public.Kudurru
	if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		return err
	}
	settings.Labels = map[string]string{"command-test": "committed"}
	_, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
	return err
}

func TestServingCurrentCommandOriginalHTTPLeaseAndWriter(t *testing.T) {
	f := newServingCommandFixture(t)
	called := false
	if err := f.s.currentCommand(t.Context(), commandSettingsWrite, func(context.Context, *sqlite.CommandTx, auth.Principal) error { called = true; return nil }); err == nil || called {
		t.Fatal("command accepted without original HTTP proof", err)
	}
	var held context.Context
	response := httptest.NewRecorder()
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		held = r.Context()
		err := f.s.currentCommand(held, commandSettingsWrite, func(ctx context.Context, tx *sqlite.CommandTx, p auth.Principal) error {
			if p.ID != "local:root-command" || p.AccountUID == "" || !p.BuiltinAdmin {
				return errors.New("current proof did not resolve the actual account")
			}
			return stageCommandLabel(ctx, tx, p)
		})
		if err != nil {
			t.Fatal("current command failed", err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		if err := f.s.currentCommand(held, commandSettingsWrite, func(context.Context, *sqlite.CommandTx, auth.Principal) error { called = true; return nil }); err == nil || called {
			t.Fatal("new publication repaired the old HTTP lease", err)
		}
	})).ServeHTTP(response, f.request())
	if response.Code != http.StatusOK || held == nil {
		t.Fatal("HTTP authentication failed", response.Code)
	}
	if err := f.s.currentCommand(held, commandSettingsWrite, stageCommandLabel); err == nil {
		t.Fatal("released HTTP request accepted")
	}
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := f.s.currentCommand(r.Context(), commandSettingsWrite, func(context.Context, *sqlite.CommandTx, auth.Principal) error { called = true; return nil }); err != nil || !called {
			t.Fatal("fresh no-op command failed", err)
		}
		f.stop()
		called = false
		if err := f.s.currentCommand(r.Context(), commandSettingsWrite, func(context.Context, *sqlite.CommandTx, auth.Principal) error { called = true; return nil }); err == nil || called {
			t.Fatal("stopped original writer admitted command", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
}

func TestServingCurrentCommandFinalCredentialClock(t *testing.T) {
	for _, changed := range []bool{false, true} {
		for _, rollback := range []bool{false, true} {
			name := "expiry"
			if rollback {
				name = "clock-regression"
			}
			if changed {
				name += "/changed"
			} else {
				name += "/noop"
			}
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				called := false
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					err := f.s.currentCommand(r.Context(), commandSettingsWrite, func(ctx context.Context, tx *sqlite.CommandTx, p auth.Principal) error {
						called = true
						if changed {
							if err := stageCommandLabel(ctx, tx, p); err != nil {
								return err
							}
						}
						if rollback {
							f.offset.Store(int64(-time.Nanosecond))
						} else {
							f.offset.Store(int64(f.expires.Sub(f.now)))
						}
						return nil
					})
					if !errors.Is(err, servingauth.ErrCredentials) {
						t.Error("invalid final credential time accepted", err)
					}
				})).ServeHTTP(httptest.NewRecorder(), f.request())
				if !called {
					t.Fatal("test did not reach authenticated command callback")
				}
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !before.Generation().Same(after.Generation()) {
					t.Fatal("failed final auth changed durable state", err)
				}
			})
		}
	}
}

func TestServingCurrentCommandHoldsLowerPinThroughFinalCheck(t *testing.T) {
	f := newServingCommandFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	original := f.s.opts.Keyrings.Capture()
	done := make(chan error, 1)
	go func() {
		response := httptest.NewRecorder()
		var commandErr error
		called := false
		f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			commandErr = f.s.currentCommand(r.Context(), commandSettingsWrite, func(ctx context.Context, tx *sqlite.CommandTx, p auth.Principal) error {
				if err := stageCommandLabel(ctx, tx, p); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(ctx context.Context) error {
					close(entered)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
					}
					if !original.Current() {
						return errors.New("lower keyring pin ended before SQLite commit")
					}
					return nil
				})
			})
		})).ServeHTTP(response, f.request())
		if !called {
			commandErr = errors.New("HTTP handler did not reach command")
		}
		done <- commandErr
	}()
	select {
	case err := <-done:
		t.Fatal("command ended before final check", err)
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("command did not reach final check")
	}
	// Even identical key bytes are a new installation. A bounded attempt must
	// time out while the actual final command check holds the lower owner gate.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	err := f.s.opts.Keyrings.Replace(ctx, original.Keys())
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !original.Current() {
		t.Fatal("replacement bypassed current-command pin", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Keyrings.Replace(t.Context(), original.Keys()); err != nil || original.Current() {
		t.Fatal("replacement failed after complete command", err)
	}
}

func TestServingCurrentCommandOriginalWriterCanceledAfterAuth(t *testing.T) {
	f := newServingCommandFixture(t)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		err := f.s.currentCommand(r.Context(), commandSettingsWrite, func(ctx context.Context, tx *sqlite.CommandTx, p auth.Principal) error {
			called = true
			if err := stageCommandLabel(ctx, tx, p); err != nil {
				return err
			}
			f.cancel() // Original elector, not just the derived HTTP context.
			return nil
		})
		if err == nil {
			t.Error("canceled original writer committed staged mutation")
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called {
		t.Fatal("test did not reach authenticated command")
	}
	f.stop()
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("canceled command changed durable image", err)
	}
}
