package servingbuild

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type blockingReaderFunc func(context.Context, time.Time) (bool, bool, time.Time, error)

func (f blockingReaderFunc) BlockingState(ctx context.Context, now time.Time) (bool, bool, time.Time, error) {
	return f(ctx, now)
}

func blockingReadHTTP(t *testing.T, f *fixture, p *serving.Publisher, read apihttp.BlockingReadSource, now func() time.Time) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: p, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	// A competing legacy view is deliberately configured off. Complete GETs
	// must use the authenticated original owner instead of either legacy input.
	cfg := *f.deps.Host
	cfg.Blocking.Enabled = false
	legacy, err := blockingstage.New(blockingstage.Options{Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: &cfg,
		Authenticate: auth.Middleware, Authorizer: auth, CrossOrigin: cop, BlockingReads: read, Blocking: legacy,
		Now: now, NodeName: "captured-node"})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(api.Handler())
	t.Cleanup(s.Close)
	return s
}

func applyBlockingReadState(t *testing.T, f *fixture, configured bool, until time.Time) {
	t.Helper()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		key := store.Key{Kind: "Kudurru", Name: "default"}
		var settings public.Kudurru
		if err := tx.Get(ctx, key, &settings); err != nil {
			return err
		}
		raw := settings.Spec.Settings["blocking"]
		var document map[string]any
		if err := json.Unmarshal(raw.Raw, &document); err != nil {
			return err
		}
		document["enabled"] = configured
		var err error
		raw.Raw, err = json.Marshal(document)
		if err != nil {
			return err
		}
		settings.Spec.Settings["blocking"] = raw
		if _, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		if err := tx.Get(ctx, key, &settings); err != nil {
			return err
		}
		settings.Status.Blocking = public.BlockingStatus{Disabled: !until.IsZero()}
		if !until.IsZero() && until.Before(blockingstage.Forever) {
			settings.Status.Blocking.DisabledUntil = &metav1.Time{Time: until}
		}
		return tx.UpdateStatus(ctx, &settings)
	})
}

func blockingReadWait(t *testing.T, p *serving.Publisher, now time.Time, configured bool, until time.Time) serving.Identity {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := p.Subscribe()
		lease, err := p.CaptureHTTP(t.Context())
		if err == nil {
			value, ok := lease.Bundle().Component("blocking")
			view, typed := value.(*blockingstage.View)
			identity := lease.Bundle().Identity()
			var matches bool
			if ok && typed {
				gotConfigured, _, gotUntil, err := view.StateAt(now)
				matches = err == nil && gotConfigured == configured && gotUntil.Equal(until)
			}
			lease.Release()
			if matches {
				return identity
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("canonical blocking state did not publish: %+v", p.Status())
		}
	}
}

func blockingReadState(t *testing.T, response objectReadResponse) apihttp.BlockingState {
	t.Helper()
	body := diagnosticWant(t, response.diagnosticResponse, http.StatusOK)
	var out apihttp.BlockingState
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err, body)
	}
	return out
}

func TestCapturedBlockingActualSQLiteHeldPublicationAndDependencyRefresh(t *testing.T) {
	for _, change := range []string{"canonical-settings-and-status", "same-image-dependency-refresh"} {
		t.Run(change, func(t *testing.T) {
			f := newCatalogFixture(t)
			until := f.now.Add(time.Minute).Truncate(time.Second)
			// Kubernetes metav1.Time serializes whole seconds. Use that exact
			// persisted boundary so acceptance measures the canonical receipt.
			now := until.Add(-time.Minute)
			applyBlockingReadState(t, f.fixture, true, until)
			before := blockingReadWait(t, f.publisher, now, true, until)
			r := NewBlockingReads()
			entered, resume := make(chan context.Context, 1), make(chan struct{})
			var hold atomic.Bool
			hold.Store(true)
			read := blockingReaderFunc(func(ctx context.Context, observed time.Time) (bool, bool, time.Time, error) {
				if hold.CompareAndSwap(true, false) {
					entered <- ctx
					select {
					case <-resume:
					case <-ctx.Done():
						return false, false, time.Time{}, ctx.Err()
					}
				}
				return r.BlockingState(ctx, observed)
			})
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			server := blockingReadHTTP(t, f.fixture, f.publisher, read, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
			t.Cleanup(func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
			})
			done := make(chan objectReadResponse, 1)
			go func() { done <- objectReadGET(t.Context(), server, f.token, "dns/blocking") }()
			var original context.Context
			select {
			case original = <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("blocking GET did not hold its original authenticated lease")
			}
			if configured, enabled, raw, err := r.BlockingState(original, until); err != nil || !configured || !enabled || !raw.Equal(until) {
				t.Fatal("held view did not expire at its absolute boundary", configured, enabled, raw, err)
			}
			if configured, enabled, raw, err := r.BlockingState(original, until.Add(-time.Nanosecond)); err != nil || !configured || enabled || !raw.Equal(until) {
				t.Fatal("later observation rewrote held view's raw deadline", configured, enabled, raw, err)
			}
			if change == "canonical-settings-and-status" {
				applyBlockingReadState(t, f.fixture, false, blockingstage.Forever)
				after := blockingReadWait(t, f.publisher, now, false, blockingstage.Forever)
				if before.Base.Same(after.Base) {
					t.Fatal("canonical settings/status change did not replace source generation")
				}
			} else {
				c := catalogDocument(t, "beta", f.url)
				f.holder.Set(c)
				after := catalogReadWait(t, f.publisher, c)
				if !before.Base.Same(after.Base) || before == after {
					t.Fatal("dependency refresh did not replace same-image installation", before, after)
				}
			}
			clock.Store(now.Add(30 * time.Second).UnixNano())
			fresh := blockingReadState(t, objectReadGET(t.Context(), server, f.token, "dns/blocking"))
			if fresh.Scope != "cluster" || fresh.Node != "captured-node" || fresh.Blocking {
				t.Fatal("fresh GET mixed legacy toggle or scope", fresh)
			}
			if change == "canonical-settings-and-status" {
				if fresh.Configured || fresh.Until != nil || fresh.Timer != nil {
					t.Fatal("fresh GET did not use successor configured-off state", fresh)
				}
			} else if !fresh.Configured || fresh.Timer == nil || *fresh.Timer != 30 || fresh.Until == nil || !fresh.Until.Equal(until) {
				t.Fatal("same-image refresh restarted or changed absolute countdown", fresh)
			}
			close(resume)
			held := blockingReadState(t, <-done)
			if !held.Configured || held.Blocking || held.Timer == nil || *held.Timer != 60 || held.Until == nil || !held.Until.Equal(until) || held.Scope != "cluster" {
				t.Fatal("held GET mixed successor state or a second observation time", held)
			}
			if _, _, _, err := r.BlockingState(original, now); err == nil {
				t.Fatal("completed handler retained usable original lease")
			}
			if change == "same-image-dependency-refresh" {
				var beforeRead, afterRead public.Kudurru
				key := store.Key{Kind: "Kudurru", Name: "default"}
				if err := f.store.Get(t.Context(), key, &beforeRead); err != nil {
					t.Fatal(err)
				}
				clock.Store(until.UnixNano())
				expired := blockingReadState(t, objectReadGET(t.Context(), server, f.token, "dns/blocking"))
				if !expired.Blocking || !expired.Configured || expired.Timer != nil || expired.Until != nil {
					t.Fatal("exact expiry required a write or produced a resume timer", expired)
				}
				if err := f.store.Get(t.Context(), key, &afterRead); err != nil {
					t.Fatal(err)
				}
				if afterRead.ResourceVersion != beforeRead.ResourceVersion || !afterRead.Status.Blocking.Disabled || afterRead.Status.Blocking.DisabledUntil == nil || !afterRead.Status.Blocking.DisabledUntil.Time.Equal(until) {
					t.Fatal("GET changed the durable absolute blocking receipt", afterRead.Status)
				}
			}
		})
	}
}

func TestCapturedBlockingActualSQLiteUnavailableOriginalOwner(t *testing.T) {
	f := newFixture(t)
	r := NewBlockingReads()
	var nilReader *BlockingReads
	for _, read := range []blockingReaderFunc{
		nilReader.BlockingState,
		func(context.Context, time.Time) (bool, bool, time.Time, error) {
			return r.BlockingState(nil, f.now) //nolint:staticcheck // Deliberate missing original context.
		},
		func(context.Context, time.Time) (bool, bool, time.Time, error) {
			return r.BlockingState(context.Background(), f.now)
		},
		func(ctx context.Context, now time.Time) (bool, bool, time.Time, error) {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			return r.BlockingState(canceled, now)
		},
		func(ctx context.Context, _ time.Time) (bool, bool, time.Time, error) {
			return r.BlockingState(ctx, time.Time{})
		},
	} {
		server := blockingReadHTTP(t, f, f.pub, read, func() time.Time { return f.now })
		diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "dns/blocking").diagnosticResponse, http.StatusServiceUnavailable)
	}
	for _, value := range []any{struct{}{}, &blockingstage.View{}} {
		// Explicit test-only malformed owner, never a production issuer.
		p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
			prepared, err := f.builder.Prepare(ctx, c)
			for i := range prepared.Components {
				if prepared.Components[i].Name == "blocking" {
					prepared.Components[i].Value = value
				}
			}
			return prepared, err
		})
		server := blockingReadHTTP(t, f, p, r, func() time.Time { return f.now })
		diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "dns/blocking").diagnosticResponse, http.StatusServiceUnavailable)
	}
}
