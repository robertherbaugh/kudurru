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

func TestCloneLeaseOriginalReplacedCutAndOwnership(t *testing.T) {
	f := newFixture(t)
	var originalCtx context.Context
	var clone *serving.Lease
	h := f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		originalCtx = r.Context()
		before, err := Identity(originalCtx)
		if err != nil {
			t.Fatal(err)
		}
		f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "new"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "new.example."}}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		f.waitCurrent(t)
		fresh, err := f.publisher.CaptureHTTP(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if before.Base.Same(fresh.Bundle().Identity().Base) {
			t.Fatal("replacement did not advance")
		}
		fresh.Release()
		clone, err = CloneLease(originalCtx)
		if err != nil || clone.Bundle().Identity() != before {
			t.Fatal("clone upgraded the original request", err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), f.request(http.MethodGet))
	if clone == nil {
		t.Fatal("handler did not clone")
	}
	defer clone.Release()
	if clone.Bundle() == nil || f.released.Load() != 0 {
		t.Fatal("returning original handler released clone owners")
	}
	if again, err := CloneLease(originalCtx); again != nil || err == nil {
		if again != nil {
			again.Release()
		}
		t.Fatal("returned request remained usable")
	}
	clone.Release()
	clone.Release()
	if f.released.Load() != 1 {
		t.Fatal("cloned owner was not released exactly once", f.released.Load())
	}
}

func TestCloneLeaseRejectsMissingReleasedAndCanceled(t *testing.T) {
	f := newFixture(t)
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ctx := range []context.Context{nil, t.Context(), context.WithValue(t.Context(), leaseKey{}, "wrong"), context.WithValue(t.Context(), leaseKey{}, lease), canceled} {
		got, err := CloneLease(ctx)
		if got != nil || err == nil {
			t.Fatal("invalid context cloned", got, err)
		}
	}
	if _, err := CloneLease(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
}

type leaseCancelContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c leaseCancelContext) Value(key any) any {
	c.cancel()
	return c.Context.Value(key)
}

func TestCloneLeaseCancellationDuringCloneDoesNotRetainOwners(t *testing.T) {
	f := newFixture(t)
	original, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer original.Release()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), leaseKey{}, original))
	defer cancel()
	clone, err := CloneLease(leaseCancelContext{Context: ctx, cancel: cancel})
	if clone != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation after cloning was ignored", clone, err)
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "new"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "new.example."}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	f.waitCurrent(t)
	original.Release()
	if f.released.Load() != 1 {
		t.Fatal("canceled clone stranded original owners", f.released.Load())
	}
}
