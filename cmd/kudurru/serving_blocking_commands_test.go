package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServingBlockingDeadlineNormalization(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 500000000, time.UTC)
	for _, tt := range []struct {
		name     string
		change   api.BlockingChange
		disabled bool
		until    time.Time
		invalid  bool
	}{
		{name: "enable-wins", change: api.BlockingChange{Blocking: true, HasUntil: true, Until: now.Add(-time.Second)}},
		{name: "indefinite", disabled: true},
		{name: "explicit-zero", change: api.BlockingChange{HasTimer: true}, disabled: true},
		{name: "round-up", change: api.BlockingChange{HasTimer: true, TimerSeconds: 1.25}, disabled: true, until: now.Truncate(time.Second).Add(2 * time.Second)},
		{name: "exact-second", change: api.BlockingChange{HasTimer: true, TimerSeconds: 0.5}, disabled: true, until: now.Truncate(time.Second).Add(time.Second)},
		{name: "until-wins", change: api.BlockingChange{HasUntil: true, Until: now.Truncate(time.Second).Add(3 * time.Second).In(time.FixedZone("test", 3600)), HasTimer: true, TimerSeconds: 30}, disabled: true, until: now.Truncate(time.Second).Add(3 * time.Second)},
		{name: "fractional-until", change: api.BlockingChange{HasUntil: true, Until: now.Add(time.Second)}, invalid: true},
		{name: "zero-until", change: api.BlockingChange{HasUntil: true}, invalid: true},
		{name: "past-until", change: api.BlockingChange{HasUntil: true, Until: now.Truncate(time.Second)}, invalid: true},
		{name: "negative", change: api.BlockingChange{HasTimer: true, TimerSeconds: -1}, invalid: true},
		{name: "nan", change: api.BlockingChange{HasTimer: true, TimerSeconds: math.NaN()}, invalid: true},
		{name: "infinity", change: api.BlockingChange{HasTimer: true, TimerSeconds: math.Inf(1)}, invalid: true},
		{name: "overflow", change: api.BlockingChange{HasTimer: true, TimerSeconds: float64(math.MaxInt64) / 1e9}, invalid: true},
		{name: "underflow", change: api.BlockingChange{HasTimer: true, TimerSeconds: 1e-10}, invalid: true},
		{name: "forever", change: api.BlockingChange{HasUntil: true, Until: blockingstage.Forever}, invalid: true},
		{name: "beyond-forever", change: api.BlockingChange{HasUntil: true, Until: blockingstage.Forever.Truncate(time.Second).Add(time.Second)}, invalid: true},
		{name: "missing-timer-flag", change: api.BlockingChange{TimerSeconds: 1}, invalid: true},
		{name: "missing-until-flag", change: api.BlockingChange{Until: now}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, err := blockingStatus(tt.change, now)
			if tt.invalid {
				var e *api.Error
				if !errors.As(err, &e) || e.Status != http.StatusBadRequest {
					t.Fatal("invalid deadline accepted", status, err)
				}
				return
			}
			if err != nil || status.Disabled != tt.disabled {
				t.Fatal(status, err)
			}
			if tt.until.IsZero() {
				if status.DisabledUntil != nil {
					t.Fatal("invented deadline", status)
				}
			} else if status.DisabledUntil == nil || !status.DisabledUntil.Equal(&metav1.Time{Time: tt.until}) || status.DisabledUntil.Nanosecond() != 0 {
				t.Fatal("wrong persisted deadline", status)
			}
		})
	}
	// Rounding may not turn a finite interval into the sentinel or beyond it.
	if _, err := blockingStatus(api.BlockingChange{HasTimer: true, TimerSeconds: 1}, blockingstage.Forever.Add(-500*time.Millisecond)); err == nil {
		t.Fatal("rounded deadline crossed maximum")
	}
}

func newBlockingCommandHTTP(t *testing.T, f *servingCommandFixture) func(string, string) *httptest.ResponseRecorder {
	t.Helper()
	legacy := *f.s.opts.Config
	legacy.Blocking.Enabled = false
	stage, err := blockingstage.New(blockingstage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: &legacy, Blocking: stage, BlockingCommand: f.s.BlockingCommand, BlockingReads: servingbuild.NewBlockingReads(), Authenticate: f.http.Middleware, Authorizer: f.http, NodeName: "blocking-node", Logger: slog.New(slog.DiscardHandler), Now: f.s.opts.Now})
	if err != nil {
		t.Fatal(err)
	}
	return func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://kudurru.test/api/v1/dns/blocking", bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+f.token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
}

func readBlockingResponse(t *testing.T, w *httptest.ResponseRecorder) api.BlockingState {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("blocking HTTP %d: %s", w.Code, w.Body)
	}
	var state api.BlockingState
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestServingBlockingActualHTTPStatusNoopAndExpiry(t *testing.T) {
	f := newServingCommandFixture(t)
	seed := blockingSettings(t, f.s.opts.Store)
	seed.Status.ObservedGeneration = 7
	seed.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Preserved", LastTransitionTime: metav1.NewTime(f.now)}}
	if err := f.s.opts.Store.UpdateStatus(t.Context(), &seed); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	request := newBlockingCommandHTTP(t, f)
	before := blockingSettings(t, f.s.opts.Store)
	enabled := readBlockingResponse(t, request(http.MethodPost, `{"blocking":true}`))
	if !enabled.Blocking || !enabled.Configured || enabled.Scope != "cluster" || enabled.Node != "blocking-node" {
		t.Fatal("live legacy values leaked", enabled)
	}
	initial, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	readBlockingResponse(t, request(http.MethodPost, `{"blocking":true}`))
	noop, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !initial.Generation().Same(noop.Generation()) {
		t.Fatal("no-op changed revision", err)
	}
	finite := readBlockingResponse(t, request(http.MethodPost, `{"blocking":false,"timer":2.25}`))
	want := f.now.Add(3 * time.Second)
	if finite.Blocking || finite.Until == nil || !finite.Until.Equal(want) || finite.Timer == nil || *finite.Timer != 3 {
		t.Fatal("timer not persisted at exact rounded deadline", finite)
	}
	stored := blockingSettings(t, f.s.opts.Store)
	if stored.UID != before.UID || !reflect.DeepEqual(stored.Spec, before.Spec) || !reflect.DeepEqual(stored.Labels, before.Labels) || !reflect.DeepEqual(stored.Annotations, before.Annotations) || stored.Status.ObservedGeneration != before.Status.ObservedGeneration || !reflect.DeepEqual(stored.Status.Conditions, before.Status.Conditions) {
		t.Fatal("status update changed unrelated singleton data")
	}
	if !stored.Status.Blocking.Disabled || stored.Status.Blocking.DisabledUntil == nil || !stored.Status.Blocking.DisabledUntil.Time.Equal(want) {
		t.Fatal("actual SQLite status missing deadline", stored.Status.Blocking)
	}
	generation, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.offset.Store(int64(3 * time.Second))
	boundary := readBlockingResponse(t, request(http.MethodGet, ""))
	if !boundary.Blocking || boundary.Timer != nil || boundary.Until != nil {
		t.Fatal("expiry boundary did not reenable", boundary)
	}
	expired, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !generation.Generation().Same(expired.Generation()) {
		t.Fatal("expiry renewed or mutated status", err)
	}
	indefinite := readBlockingResponse(t, request(http.MethodPost, `{"blocking":false,"timer":0}`))
	if indefinite.Blocking || indefinite.Timer != nil || indefinite.Until != nil {
		t.Fatal("indefinite disable fabricated timer", indefinite)
	}
	absolute := f.now.Add(20 * time.Second)
	payload := `{"blocking":false,"timer":300,"until":"` + absolute.Format(time.RFC3339) + `"}`
	explicit := readBlockingResponse(t, request(http.MethodPost, payload))
	if explicit.Until == nil || !explicit.Until.Equal(absolute) {
		t.Fatal("until did not override timer", explicit)
	}
	current, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	readBlockingResponse(t, request(http.MethodPost, payload))
	unchanged, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !current.Generation().Same(unchanged.Generation()) {
		t.Fatal("absolute no-op changed revision", err)
	}
	readBlockingResponse(t, request(http.MethodPost, `{"blocking":true}`))
	if got := blockingSettings(t, f.s.opts.Store); got.Status.Blocking.Disabled || got.Status.Blocking.DisabledUntil != nil {
		t.Fatal("enable retained deadline")
	}
	if got := request(http.MethodPost, `{"blocking":false,"until":"`+f.now.Add(time.Nanosecond).Format(time.RFC3339Nano)+`"}`); got.Code != http.StatusBadRequest {
		t.Fatal("fractional absolute deadline accepted", got.Code, got.Body)
	}
}

func blockingSettings(t *testing.T, st *sqlite.Store) public.Kudurru {
	t.Helper()
	var settings public.Kudurru
	if err := st.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestServingBlockingCurrentProofFinalClockAndRollback(t *testing.T) {
	for _, changed := range []bool{false, true} {
		for _, backwards := range []bool{false, true} {
			name := "noop"
			if changed {
				name = "changed"
			}
			if backwards {
				name += "-backwards"
			} else {
				name += "-expiry"
			}
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				called := false
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					called = true
					// The adapter captured its clock at construction. Replacing the command
					// clock here isolates raw current authentication/operation/final checks.
					original := f.s.opts.Now
					var calls atomic.Int64
					f.s.opts.Now = func() time.Time {
						if calls.Add(1) >= 3 {
							if backwards {
								return f.now.Add(-time.Nanosecond)
							}
							return f.expires
						}
						return f.now
					}
					_, err := f.s.BlockingCommand(r.Context(), api.BlockingChange{Blocking: !changed})
					f.s.opts.Now = original
					var e *api.Error
					if !errors.As(err, &e) || e.Status != http.StatusUnauthorized || e.Body.Hint != nil {
						t.Error("invalid final proof did not roll back", err)
					}
				})).ServeHTTP(httptest.NewRecorder(), f.request())
				if !called {
					t.Fatal("HTTP handler not reached")
				}
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !before.Generation().Same(after.Generation()) {
					t.Fatal("final proof failure mutated durable image", err)
				}
			})
		}
	}
}

func TestServingBlockingPostcommitHelperOutcomesAndPublication(t *testing.T) {
	f := newServingCommandFixture(t)
	for _, mode := range []string{"rollback", "noop", "write", "canceled-after"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			called := false
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				proof, err := servinghttp.Credential(r.Context())
				if err != nil {
					t.Fatal(err)
				}
				var receipt *public.Kudurru
				failure := errors.New("deliberate rollback")
				committed, err := f.s.currentAuthCommandThen(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
					if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, blockingToggleAttrs, f.s.opts.Now); err != nil {
						return err
					}
					var settings public.Kudurru
					if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
						return err
					}
					if mode != "noop" {
						settings.Status.Blocking = public.BlockingStatus{Disabled: true}
						if err := tx.UpdateStatus(ctx, &settings); err != nil {
							return err
						}
					}
					if mode == "rollback" {
						return failure
					}
					if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
						return err
					}
					receipt = settings.DeepCopy()
					return nil
				}, func(ctx context.Context, target serving.TermActivation) error {
					calls++
					// A fresh publisher observation needs its DB/coordinator/host pins. It
					// cannot converge if this callback still runs under the command locks.
					original, err := servinghttp.Component[*servingauth.View](r.Context(), "auth")
					if err != nil {
						return err
					}
					owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](r.Context(), "runtime")
					if err != nil {
						return err
					}
					_, _, current := owner.NotifyKeys()
					f.s.publisher.Wake()
					if _, err := f.s.awaitBlockingPublication(ctx, target, receipt, original.DependencyIdentity(), current, f.now); err != nil {
						return err
					}
					if mode == "canceled-after" {
						f.cancel()
					}
					return nil
				})
				switch mode {
				case "rollback":
					if committed || !errors.Is(err, failure) || calls != 0 {
						t.Error("failed command ran after callback", committed, err, calls)
					}
				case "canceled-after":
					if !committed || err == nil || calls != 1 {
						t.Error("postcommit cancellation claimed rollback", committed, err, calls)
					}
				default:
					if !committed || err != nil || calls != 1 {
						t.Error("successful command not synchronously acknowledged", committed, err, calls)
					}
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if !called {
				t.Fatal("HTTP admission unavailable", f.s.publisher.Status())
			}
		})
	}
}

func TestServingBlockingFollowerRestartPromotionPreservesAbsoluteStatus(t *testing.T) {
	f := newServingCommandFixture(t)
	request := newBlockingCommandHTTP(t, f)
	readBlockingResponse(t, request(http.MethodPost, `{"blocking":false,"timer":30}`))
	original := blockingSettings(t, f.s.opts.Store)
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "follower.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "blocking-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	defer func() {
		if err := follower.Close(); err != nil {
			t.Error(err)
		}
	}()
	if got := blockingSettings(t, follower); !reflect.DeepEqual(got.Status, original.Status) || got.UID != original.UID || got.ResourceVersion != original.ResourceVersion {
		t.Fatal("restart changed absolute status")
	}
	promoted, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "blocking-absolute-promotion"})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Term.Same(image.Term) {
		t.Fatal("promotion did not create a new term")
	}
	if got := blockingSettings(t, follower); !reflect.DeepEqual(got.Status, original.Status) || got.ResourceVersion != original.ResourceVersion {
		t.Fatal("promotion renewed deadline")
	}
}

func TestServingBlockingCommittedPendingReceiptAndSupersession(t *testing.T) {
	f := newServingCommandFixture(t)
	var commandErr error
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		original := f.s.opts.Now
		var calls atomic.Int64
		f.s.opts.Now = func() time.Time {
			// Authenticate, resolve deadline and finish the detached commit check at
			// the valid time. Reject only the postcommit publication observation.
			if calls.Add(1) >= 4 {
				return f.now.Add(-time.Nanosecond)
			}
			return f.now
		}
		_, commandErr = f.s.BlockingCommand(r.Context(), api.BlockingChange{HasTimer: true, TimerSeconds: 30})
		f.s.opts.Now = original
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called {
		t.Fatal("HTTP handler unavailable")
	}
	var failure *api.Error
	if !errors.As(commandErr, &failure) || failure.Status != http.StatusServiceUnavailable {
		t.Fatal("publication failure returned success", commandErr)
	}
	receipt, ok := failure.Body.Hint.(blockingReceipt)
	if !ok || !receipt.Committed || receipt.Publication != "pending" || receipt.ResourceVersion == "" || receipt.SettingsUID == "" || !receipt.Disabled || receipt.DisabledUntil == nil || !receipt.DisabledUntil.Equal(f.now.Add(30*time.Second)) {
		t.Fatal("lost exact committed receipt", failure.Body.Hint)
	}
	stored := blockingSettings(t, f.s.opts.Store)
	if string(stored.UID) != receipt.SettingsUID || stored.ResourceVersion != receipt.ResourceVersion || stored.Status.Blocking.DisabledUntil == nil || !stored.Status.Blocking.DisabledUntil.Time.Equal(*receipt.DisabledUntil) {
		t.Fatal("pending receipt claimed rollback or renewed deadline")
	}
	// A fresh request observes the durable deadline; the failed acknowledgment
	// neither retries the command nor mutates legacy state.
	request := newBlockingCommandHTTP(t, f)
	state := readBlockingResponse(t, request(http.MethodGet, ""))
	if state.Blocking || state.Until == nil || !state.Until.Equal(*receipt.DisabledUntil) {
		t.Fatal("committed status missing from publication", state)
	}
}

func TestServingBlockingConfiguredOffAndHeldOriginalScope(t *testing.T) {
	f := newServingCommandFixture(t)
	// Write the canonical flag through its own actual current command. Changing
	// a competing legacy toggle/config is insufficient in complete serving mode.
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		cfg, err := f.s.Settings(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Blocking.Enabled = false
		if err := f.s.WriteSettings(r.Context(), cfg, []string{"blocking.enabled"}); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called {
		t.Fatal("settings HTTP unavailable")
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	request := newBlockingCommandHTTP(t, f)
	state := readBlockingResponse(t, request(http.MethodPost, `{"blocking":false,"timer":10}`))
	if state.Blocking || state.Configured || state.Timer != nil || state.Until != nil {
		t.Fatal("configured-off returned effective resume timer", state)
	}
	if stored := blockingSettings(t, f.s.opts.Store); stored.Status.Blocking.DisabledUntil == nil || !stored.Status.Blocking.DisabledUntil.Time.Equal(f.now.Add(10*time.Second)) {
		t.Fatal("configured-off lost durable absolute deadline")
	}
	state = readBlockingResponse(t, request(http.MethodPost, `{"blocking":true}`))
	if state.Blocking || state.Configured || state.Timer != nil || state.Until != nil {
		t.Fatal("enable overrode configured-off", state)
	}
	var held context.Context
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		held = r.Context()
		if _, err := f.s.BlockingCommand(held, api.BlockingChange{}); err != nil {
			t.Fatal(err)
		}
		// A command's own revision invalidates the old request for further writes.
		if _, err := f.s.BlockingCommand(held, api.BlockingChange{Blocking: true}); err == nil {
			t.Error("held original request adopted successor")
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if held == nil {
		t.Fatal("HTTP callback unavailable")
	}
	if _, err := f.s.BlockingCommand(held, api.BlockingChange{Blocking: true}); err == nil {
		t.Fatal("released request accepted")
	}
}

func TestServingBlockingCurrentRawScopeAndCanceledWriter(t *testing.T) {
	for _, mode := range []string{"raw-scope", "canceled-writer", "canceled-request"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				ctx := r.Context()
				switch mode {
				case "raw-scope":
					// Replace current token scope after route authentication. The old
					// request must fail admission, never repair itself with a new lease.
					if err := f.s.opts.Store.Command(ctx, f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						objects, err := tx.List(ctx, "APIToken", store.ListOptions{})
						if err != nil {
							return err
						}
						for _, object := range objects.Items {
							token := object.(*internalv1.APIToken)
							token.Spec.Scopes = []string{"viewer"}
							if _, err := tx.Apply(ctx, token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				case "canceled-writer":
					f.cancel()
				default:
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				_, err := f.s.BlockingCommand(ctx, api.BlockingChange{})
				if err == nil {
					t.Error("changed authority or canceled request admitted")
				}
				if mode != "raw-scope" {
					var e *api.Error
					if errors.As(err, &e) && e.Body.Hint != nil {
						t.Error("uncommitted cancellation fabricated receipt", err)
					}
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if !called {
				t.Fatal("HTTP handler unavailable")
			}
			stored := blockingSettings(t, f.s.opts.Store)
			if stored.Status.Blocking.Disabled || stored.Status.Blocking.DisabledUntil != nil {
				t.Fatal("refused command changed status")
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if mode != "raw-scope" && !before.Generation().Same(after.Generation()) {
				t.Fatal("cancellation changed durable generation")
			}
		})
	}
}

func TestServingBlockingMissingCanonicalSingletonIsUnavailable(t *testing.T) {
	f := newServingCommandFixture(t)
	called := false
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		proof, err := servinghttp.Credential(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		afterCalls := 0
		committed, err := f.s.currentAuthCommandThen(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, blockingToggleAttrs, f.s.opts.Now); err != nil {
				return err
			}
			// Stage absence in the exact current transaction. No malformed serving
			// bundle is fabricated and the refusal must roll this removal back too.
			if err := tx.Delete(ctx, store.Key{Kind: "Kudurru", Name: "default"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
				return err
			}
			_, err := blockingCommandSettings(ctx, tx)
			return err
		}, func(context.Context, serving.TermActivation) error { afterCalls++; return nil })
		var failure *api.Error
		if committed || afterCalls != 0 || !errors.As(currentCommandAPIError(err), &failure) || failure.Status != http.StatusServiceUnavailable || failure.Body.Key != api.KeyUnavailable || failure.Body.Hint != nil {
			t.Error("missing singleton was not an uncommitted503", committed, err, afterCalls)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called {
		t.Fatal("HTTP handler unavailable")
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("missing singleton refusal changed durable data", err)
	}
	_ = blockingSettings(t, f.s.opts.Store)
}

func TestServingBlockingPostcommitExpiryAndExactReceipt(t *testing.T) {
	f := newServingCommandFixture(t)
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		proof, err := servinghttp.Credential(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		original, err := servinghttp.Component[*servingauth.View](r.Context(), "auth")
		if err != nil {
			t.Fatal(err)
		}
		owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](r.Context(), "runtime")
		if err != nil {
			t.Fatal(err)
		}
		_, _, current := owner.NotifyKeys()
		var receipt *public.Kudurru
		committed, err := f.s.currentAuthCommandThen(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, blockingToggleAttrs, f.s.opts.Now); err != nil {
				return err
			}
			settings, err := blockingCommandSettings(ctx, tx)
			if err != nil {
				return err
			}
			settings.Status.Blocking, err = blockingStatus(api.BlockingChange{HasTimer: true, TimerSeconds: 1}, f.now)
			if err != nil {
				return err
			}
			if err := tx.UpdateStatus(ctx, settings); err != nil {
				return err
			}
			receipt, err = blockingCommandSettings(ctx, tx)
			return err
		}, func(ctx context.Context, target serving.TermActivation) error {
			f.offset.Store(int64(time.Second)) // Exact deadline before acknowledgment.
			f.s.publisher.Wake()
			result, err := f.s.awaitBlockingPublication(ctx, target, receipt, original.DependencyIdentity(), current, f.now)
			if err != nil {
				return err
			}
			if !result.Blocking || result.Until != nil || result.Timer != nil {
				t.Error("expired original timer renewed during acknowledgment", result)
			}
			lease, err := f.s.publisher.CaptureHTTP(ctx)
			if err != nil {
				return err
			}
			defer lease.Release()
			for _, field := range []string{"uid", "rv", "status", "dependencies", "current"} {
				fake := receipt.DeepCopy()
				dependency := original.DependencyIdentity()
				predicate := current
				switch field {
				case "uid":
					fake.UID = "different"
				case "rv":
					fake.ResourceVersion = "different"
				case "status":
					fake.Status.Blocking.Disabled = false
				case "dependencies":
					dependency = "different"
				case "current":
					predicate = func() bool { return false }
				}
				result, ready, err := f.s.blockingPublication(ctx, lease, target, fake, dependency, predicate, f.now)
				if ready || result != nil {
					t.Error("acknowledged mismatched receipt/dependency", field, result)
				}
				if (field == "dependencies" || field == "current") && !errors.Is(err, serving.ErrDependency) {
					t.Error("lost dependency gate", field, err)
				}
			}
			return nil
		})
		if !committed || err != nil {
			t.Error("expiry observation failed", committed, err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called {
		t.Fatal("HTTP handler unavailable")
	}
	stored := blockingSettings(t, f.s.opts.Store)
	if stored.Status.Blocking.DisabledUntil == nil || !stored.Status.Blocking.DisabledUntil.Time.Equal(f.now.Add(time.Second)) {
		t.Fatal("acknowledgment renewed timer")
	}
}

func TestServingBlockingWriterCanceledAfterCommitBeforeObservation(t *testing.T) {
	for _, mode := range []string{"helper", "blocking"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			scheme, err := runtimekinds.NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			schema, err := servingbuild.NewSchema(scheme, f.s.opts.Validator, runtimeowners.ValidateEntry, runtimeowners.ValidateImage)
			if err != nil {
				t.Fatal(err)
			}
			// Use the SAME real coordinator/current host guard and SQLite store. The
			// narrow wrapper cancels only after the actual Command returned from the
			// full host pin: this deterministically exercises pre-callback cancellation.
			coordinator, err := providercoordinator.New(providercoordinator.Options{
				Store: f.s.opts.Store, Publisher: f.s.publisher, Resources: f.s.resources,
				Schema: servingauth.Schema{Scheme: scheme, Kinds: schema.PrimaryImageOptions(t.Context(), f.s.opts.Store.Info().ClusterID).Validation.Kinds, SchemaVersion: store.SchemaVersion},
				Host:   f.s.host.Host, HostChanges: f.s.host.Changes, Admit: f.s.host.CheckAdmission, Now: f.s.opts.Now,
				PinHost: func(ctx context.Context, fn func() error) error {
					err := f.s.pinProviderHost(ctx, fn)
					if err == nil {
						f.cancel()
					}
					return err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := coordinator.Close(t.Context()); err != nil {
					t.Error(err)
				}
			})
			commandRuntime := &servingRuntime{opts: f.s.opts, primary: f.s.primary, publisher: f.s.publisher, providers: coordinator}
			called := false
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				if mode == "blocking" {
					_, err := commandRuntime.BlockingCommand(r.Context(), api.BlockingChange{})
					var failure *api.Error
					if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
						t.Error("postcommit cancellation returned false success", err)
						return
					}
					receipt, ok := failure.Body.Hint.(blockingReceipt)
					if !ok || !receipt.Committed || receipt.ResourceVersion == "" || !receipt.Disabled || receipt.Publication != "pending" {
						t.Error("committed write mislabeled rollback", failure.Body.Hint)
					}
					return
				}
				proof, err := servinghttp.Credential(r.Context())
				if err != nil {
					t.Fatal(err)
				}
				callbacks := 0
				committed, err := commandRuntime.currentAuthCommandThen(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
					if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, blockingToggleAttrs, f.s.opts.Now); err != nil {
						return err
					}
					settings, err := blockingCommandSettings(ctx, tx)
					if err != nil {
						return err
					}
					settings.Status.Blocking.Disabled = true
					return tx.UpdateStatus(ctx, settings)
				}, func(context.Context, serving.TermActivation) error { callbacks++; return nil })
				if !committed || err == nil || callbacks != 0 {
					t.Error("pre-entry writer cancellation lost committed outcome", committed, err, callbacks)
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if !called {
				t.Fatal("HTTP handler unavailable")
			}
			if stored := blockingSettings(t, f.s.opts.Store); !stored.Status.Blocking.Disabled {
				t.Fatal("committed status falsely reported as rollback")
			}
		})
	}
}
