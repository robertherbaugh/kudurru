package servinghttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSourceTokenRetainsOriginalAfterReplacement(t *testing.T) {
	f := newFixture(t)
	var original context.Context
	h := f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		original = r.Context()
		before, err := SourceToken(original)
		if err != nil {
			t.Fatal(err)
		}
		f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "replacement"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "replacement.example."}}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		f.waitCurrent(t)
		after, err := SourceToken(original)
		if err != nil || !after.Generation().Same(before.Generation()) || !after.Matches(before.Authority()) {
			t.Fatal("source upgraded original request", err)
		}
		if after.Matches(f.store.Authority()) {
			t.Fatal("old source falsely matches replacement")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), f.request(http.MethodGet))
	if _, err := SourceToken(original); err == nil {
		t.Fatal("returned request supplied source")
	}
}

func TestSourceTokenRefusesReleasedAndCanceled(t *testing.T) {
	f := newFixture(t)
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), leaseKey{}, lease))
	if _, err := SourceToken(leaseCancelContext{Context: ctx, cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatal("source ignored cancellation during lookup", err)
	}
	lease.Release()
	for _, c := range []context.Context{nil, t.Context(), context.WithValue(t.Context(), leaseKey{}, lease), context.WithValue(t.Context(), leaseKey{}, &serving.Lease{})} {
		if _, err := SourceToken(c); !errors.Is(err, serving.ErrReleased) {
			t.Fatal("invalid lease supplied source", err)
		}
	}
}

// The mode of the original source stays readable after the request's context
// has ended, but not after the lease is released, and it is never a token.
func TestSourceModeAnswersForAnEndedRequestUntilRelease(t *testing.T) {
	f := newFixture(t)
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), leaseKey{}, lease))
	want := f.store.Authority().Mode
	if mode, err := SourceMode(ctx); err != nil || mode != want || want == "" {
		t.Fatal("live request did not report its source mode", mode, err)
	}
	cancel()
	if _, err := SourceToken(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("ended request supplied a source token", err)
	}
	if mode, err := SourceMode(ctx); err != nil || mode != want {
		t.Fatal("ended request lost its source mode", mode, err)
	}
	lease.Release()
	for _, c := range []context.Context{nil, t.Context(), ctx, context.WithValue(t.Context(), leaseKey{}, &serving.Lease{})} {
		if mode, err := SourceMode(c); !errors.Is(err, serving.ErrReleased) || mode != "" {
			t.Fatal("invalid lease supplied a source mode", mode, err)
		}
	}
}
