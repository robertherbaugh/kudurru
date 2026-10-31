package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingNotifySeed(t *testing.T, f *servingCommandFixture, mode public.NotifyMode, expiry bool) {
	t.Helper()
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "notify"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "manual.test.", Nameservers: []string{"ns.manual.test."}, Policy: public.ZonePolicy{Notify: mode, NotifyTargets: []string{"127.0.0.1:9"}}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if expiry {
		deadline := metav1.NewTime(f.now.Add(30 * time.Minute))
		rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "expiring", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "notify"}, Endpoints: []public.Endpoint{{DNSName: "transient.manual.test.", RecordType: "A", Targets: []string{"192.0.2.9"}, Kudurru: &public.EndpointExt{ExpiresAt: &deadline}}}}}
		if _, err := f.s.opts.Store.Apply(t.Context(), rs, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		v, ok := lease.Bundle().Component("runtime")
		if !ok {
			return false
		}
		r := v.(*servingbuild.RuntimeOwner)
		return f.s.primary.driver.Status().Ready && r.Primary().Admitted("manual.test.", f.now)
	})
}

func servingNotifyAPI(t *testing.T, f *servingCommandFixture) *api.Server {
	t.Helper()
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, ZoneNotifyCommand: f.s.ZoneNotifyCommand, Authenticate: f.http.Middleware, Authorizer: f.http, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func notifyAPIStatus(t *testing.T, err error, want int) {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Status != want {
		t.Fatalf("status %d: %v", want, err)
	}
}

func TestServingZoneNotifyPolicyNoneCurrentScopeAndUnavailable(t *testing.T) {
	f := newServingCommandFixture(t)
	servingNotifySeed(t, f, public.NotifyNone, false)
	id, _ := tokens.Parse(f.token)
	var token internalv1.APIToken
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
		t.Fatal(err)
	}
	token.Spec.Scopes = []string{"zone-admin@zone:notify"}
	if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	srv := servingNotifyAPI(t, f)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/notify/notify", nil)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"targets":[]`) {
		t.Fatal(w.Code, w.Body)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("none intent wrote durable state", err)
	}
	if w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/other/notify", nil); w.Code != http.StatusForbidden {
		t.Fatal("scope escaped", w.Code, w.Body)
	}
	if _, err := f.s.ZoneNotifyCommand(t.Context(), "notify"); err == nil {
		t.Fatal("missing original proof admitted")
	}
}

func TestServingZoneNotifyFinalChangedAndNoopAdmission(t *testing.T) {
	for _, mode := range []string{"changed-expiry", "none-expiry", "pending-expiry", "changed-backwards", "none-backwards", "pending-backwards", "changed-writer", "none-writer", "changed-deadline", "none-deadline", "changed-primary-expiry", "none-primary-expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			policy := public.NotifySpecified
			if strings.HasPrefix(mode, "none-") {
				policy = public.NotifyNone
			}
			servingNotifySeed(t, f, policy, strings.HasSuffix(mode, "primary-expiry"))
			if strings.HasPrefix(mode, "pending-") {
				srv := servingNotifyAPI(t, f)
				w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/notify/notify", nil)
				if w.Code != http.StatusAccepted {
					t.Fatal("queue initial pending intent", w.Code, w.Body)
				}
				awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady && f.s.primary.driver.Status().Ready })
			}
			original := f.s.primary.notifyIntent
			called := false
			var cancel context.CancelFunc
			f.s.primary.notifyIntent = func(ctx context.Context, tx *sqlite.CommandTx, o pr.PrimaryStatusObservation) (sqliteadapter.NotifyIntentResult, error) {
				result, err := original(ctx, tx, o)
				if err != nil {
					return result, err
				}
				called = true
				if result.Changed != strings.HasPrefix(mode, "changed-") {
					t.Fatal("fixture did not exercise changed/no-op", mode)
				}
				switch {
				case strings.HasSuffix(mode, "primary-expiry"):
					f.offset.Store(int64(30 * time.Minute))
				case strings.HasSuffix(mode, "expiry"):
					f.offset.Store(int64(f.expires.Sub(f.now)))
				case strings.HasSuffix(mode, "backwards"):
					f.offset.Store(int64(-time.Second))
				case strings.HasSuffix(mode, "writer"):
					f.cancel()
				case strings.HasSuffix(mode, "deadline"):
					cancel()
				}
				return result, nil
			}
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				if strings.HasSuffix(mode, "deadline") {
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
				}
				_, err := f.s.ZoneNotifyCommand(ctx, "notify")
				if err == nil {
					t.Error("original lifetime ended but admission succeeded")
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if !called {
				t.Fatal("actual current intent not reached")
			}
			f.stop()
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("final denial leaked metadata intent/revision", err)
			}
		})
	}
}

func TestServingZoneNotifyDeferredSemanticFinalClocks(t *testing.T) {
	for _, mode := range []string{"missing", "nonprimary"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			if mode == "nonprimary" {
				z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "semantic"}, Spec: public.DNSZoneSpec{ZoneName: "semantic.test.", Type: public.ZoneTypeForward}}
				if _, err := f.s.opts.Store.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
			}
			srv := servingNotifyAPI(t, f)
			want := http.StatusNotFound
			if mode == "nonprimary" {
				want = http.StatusConflict
			}
			w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/semantic/notify", nil)
			if w.Code != want {
				t.Fatal(w.Code, w.Body)
			}
			for _, backwards := range []bool{false, true} {
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					clock := f.s.opts.Now
					calls := 0
					f.s.opts.Now = func() time.Time {
						calls++
						if calls <= 2 {
							return f.now
						}
						if backwards {
							return f.now.Add(-time.Second)
						}
						return f.expires
					}
					defer func() { f.s.opts.Now = clock }()
					_, err := f.s.ZoneNotifyCommand(r.Context(), "semantic")
					notifyAPIStatus(t, err, http.StatusUnauthorized)
					if calls < 3 {
						t.Fatal("semantic result skipped final clock")
					}
				})).ServeHTTP(httptest.NewRecorder(), f.request())
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !before.Generation().Same(after.Generation()) {
					t.Fatal("semantic refusal wrote", err)
				}
			}
		})
	}
}

func TestServingZoneNotifyOriginalReleasedAndSupersededLease(t *testing.T) {
	f := newServingCommandFixture(t)
	servingNotifySeed(t, f, public.NotifyNone, false)
	var held context.Context
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		held = r.Context()
		var z public.DNSZone
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "notify"}, &z); err != nil {
			t.Fatal(err)
		}
		z.Spec.Policy.NotifyTargets = []string{"192.0.2.99"}
		if _, err := f.s.opts.Store.Apply(t.Context(), &z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.ZoneNotifyCommand(held, "notify")
		notifyAPIStatus(t, err, http.StatusServiceUnavailable)
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	_, err := f.s.ZoneNotifyCommand(held, "notify")
	notifyAPIStatus(t, err, http.StatusUnauthorized)
}

func TestServingZoneNotifyRejectsFallbackWithoutIntent(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	servingNotifySeed(t, f, public.NotifyNone, false)
	srv := servingNotifyAPI(t, f)
	var z public.DNSZone
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "notify"}, &z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Fallback = &public.ZoneFallbackSpec{}
	saved, err := f.s.opts.Store.Apply(t.Context(), &z, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	wantSource, err := primaryzone.SourceToken(saved)
	if err != nil {
		t.Fatal(err)
	}
	// Fallback changes the primary content hash and queues an automatic intent.
	// Even with delivery disabled, clearing that intent commits another head.
	// Wait for that exact source/head and its completed publication before the
	// single request; restore readiness alone permits these intermediate cuts.
	awaitServingRuntime(t, f.s, func() bool {
		status := f.s.publisher.Status()
		driver := f.s.primary.driver.Status()
		if !status.ProtectedReady || status.Preparing || !driver.Ready {
			return false
		}
		image, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		bundle := lease.Bundle()
		if bundle.Identity() != status.Served || !bundle.Identity().Base.Same(image.Generation()) {
			return false
		}
		component, _ := bundle.Component("runtime")
		owner, ok := component.(*servingbuild.RuntimeOwner)
		if !ok || owner.Primary() == nil || !owner.Primary().Base().Same(image.Generation()) {
			return false
		}
		o, ok := owner.Primary().CommittedPrimaryStatus("notify")
		wantLifecycleSource := zk.LifecycleSource{UID: wantSource.UID, ResourceVersion: wantSource.ResourceVersion, IntentDigest: wantSource.IntentDigest}
		if !ok || !o.ConfigMatchesDesired || o.Source != wantLifecycleSource || o.CommittedSource != wantSource || !owner.Primary().Admitted("manual.test.", f.now) {
			return false
		}
		route := owner.Zones().Zones(f.now).Match("manual.test.")
		if route == nil || route.Name() != "manual.test." || !route.HasFallback() || !route.RoutingAvailableAt(f.now) {
			return false
		}
		var head zk.ZoneState
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(o.HeadZone)}, &head); err != nil {
			t.Fatal(err)
		}
		if head.ResourceVersion != o.HeadResourceVersion || head.Spec.NotifySequence != 0 || head.Spec.NotifySerial != 0 {
			return false
		}
		acknowledged := false
		for _, zone := range driver.Zones {
			if zone.ObjectName == "notify" {
				acknowledged = zone.Installed && zone.Acknowledged && zone.Published && zone.Version == o.Member.Version && zone.Incarnation == o.Member.Incarnation && zone.Sequence == o.Member.Sequence
			}
		}
		current := f.s.publisher.Status()
		return acknowledged && current.ProtectedReady && !current.Preparing && current.Served == status.Served
	})
	f.s.primary.notifyIntent = func(context.Context, *sqlite.CommandTx, pr.PrimaryStatusObservation) (sqliteadapter.NotifyIntentResult, error) {
		t.Error("fallback must not queue a NOTIFY intent")
		return sqliteadapter.NotifyIntentResult{}, errors.New("unexpected intent")
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/notify/notify", nil)
	if w.Code != http.StatusUnprocessableEntity {
		status := f.s.publisher.Status()
		t.Fatalf("fallback NOTIFY was not refused: status=%d preparing=%t publication=%d revision=%d", w.Code, status.Preparing, status.Served.Publication, status.Served.Base.Revision)
	}
	f.stop()
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("fallback refusal changed durable state", err)
	}
}
