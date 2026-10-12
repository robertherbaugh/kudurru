package listfetcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// TestRefreshNow: an on-demand refresh downloads a list's sources at the
// next pass although they are not due, and bypasses a failure cooldown.
func TestRefreshNow(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: srv.URL + "/hosts"}, v1alpha1.ListSource{Inline: []string{"||inline.example^"}})
	ctx := context.Background()

	fx.f.Reconcile(ctx)
	if srv.hits.Load() != 1 {
		t.Fatalf("first pass: %d hits", srv.hits.Load())
	}
	fx.clk.Advance(time.Minute)
	fx.f.Reconcile(ctx)
	if srv.hits.Load() != 1 {
		t.Fatalf("not due: %d hits", srv.hits.Load())
	}
	if err := fx.f.RefreshNow("default", "ads"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fx.f.kick:
	default:
		t.Fatal("RefreshNow must wake the run loop")
	}
	fx.f.Reconcile(ctx)
	if srv.hits.Load() != 2 {
		t.Fatalf("forced: %d hits", srv.hits.Load())
	}
	fx.f.Reconcile(ctx)
	if srv.hits.Load() != 2 {
		t.Fatalf("a forced refresh is one download: %d hits", srv.hits.Load())
	}

	// A failure puts the source in cooldown; RefreshNow ignores it.
	srv.set("500", "", "")
	_ = fx.f.RefreshNow("default", "ads")
	fx.f.Reconcile(ctx)
	_ = fx.f.RefreshNow("default", "ads")
	fx.f.Reconcile(ctx)
	if srv.hits.Load() < 4 {
		t.Fatalf("cooldown not bypassed: %d hits", srv.hits.Load())
	}

	if err := fx.f.RefreshNow("default", "nope"); !errors.Is(err, ErrUnknownList) {
		t.Fatalf("unknown list: %v", err)
	}
}
