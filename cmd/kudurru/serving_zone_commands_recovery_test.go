package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestServingZoneCommandOriginalLeaseAndPlanLifetime(t *testing.T) {
	f := newServingCommandFixture(t)
	var retained context.Context
	var retainedPlan *api.ZoneCommandPlan
	var retainedTx *sqlite.CommandTx
	var retainedTarget api.ZoneCommandTarget
	command := func(ctx context.Context, target api.ZoneCommandTarget, prepare api.ZoneCommandPrepare) error {
		retained, retainedTarget = ctx, target
		err := f.s.ZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.ZoneCommandPlan, error) {
			plan, err := prepare(ctx, tx)
			retainedPlan, retainedTx = plan, tx
			return plan, err
		})
		if err != nil {
			return err
		}
		if retainedPlan.PreparedFor(retainedTx, target) || retainedPlan.Requirements() != nil || retainedPlan.Apply(ctx, retainedTx) == nil {
			t.Fatal("completed original command retained a usable plan")
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		called := false
		if err := f.s.ZoneCommand(ctx, target, func(context.Context, *sqlite.CommandTx) (*api.ZoneCommandPlan, error) {
			called = true
			return nil, nil
		}); err == nil || called {
			t.Fatal("new publication repaired the old HTTP lease", err)
		}
		return nil
	}
	srv := servingZoneAPI(t, f, command)
	w := servingZoneReadyRequest(t, f, srv, http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1), http.StatusOK)
	if w.Code != http.StatusOK || retained == nil || retainedPlan == nil {
		t.Fatal("original current import failed", w.Code, w.Body)
	}
	called := false
	if err := f.s.ZoneCommand(retained, retainedTarget, func(context.Context, *sqlite.CommandTx) (*api.ZoneCommandPlan, error) {
		called = true
		return nil, nil
	}); err == nil || called {
		t.Fatal("released original HTTP lease reached preparation", err)
	}
}

func TestServingZoneCommandOriginalRequestAndWriterCancellation(t *testing.T) {
	for _, mode := range []string{"request", "writer", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			command := func(ctx context.Context, target api.ZoneCommandTarget, prepare api.ZoneCommandPrepare) error {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if mode == "deadline" {
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				}
				return f.s.ZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*api.ZoneCommandPlan, error) {
					plan, err := prepare(ctx, tx)
					called = true
					if mode == "writer" {
						// Join only outside the command's writer exclusions.
						f.cancel()
					} else {
						cancel()
					}
					return plan, err
				})
			}
			srv := servingZoneAPI(t, f, command)
			w := servingZoneRequest(t, f, srv, http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1500))
			if w.Code < http.StatusBadRequest || called != (mode != "deadline") {
				t.Fatal("canceled original request/writer was admitted", mode, w.Code, w.Body, called)
			}
			if mode == "writer" {
				f.stop()
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("cancellation changed durable complete image", err)
			}
		})
	}
}

func TestServingZoneCommandFollowerRestartCompleteImage(t *testing.T) {
	f := newServingCommandFixture(t)
	srv := servingZoneAPI(t, f, nil)
	servingZoneReadyRequest(t, f, srv, http.MethodPost, "/api/v1/zones/compound/import?origin=compound.test.", servingZoneFile(1500), http.StatusOK)
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compound-follower.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "compound-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	t.Cleanup(func() { _ = follower.Close() })
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
	after, err := follower.ReadGeneration(t.Context())
	if err != nil || !after.Generation().Same(installed) {
		t.Fatal("restart changed installed complete import", err)
	}
	var z public.DNSZone
	if err := follower.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "compound"}, &z); err != nil || store.ManagedBy(&z) != store.ManagerAPI || z.UID == "" {
		t.Fatal("restart lost current zone identity or ownership", err)
	}
	for i, count := range []int{1000, 500} {
		var source, target public.DNSRecordSet
		key := store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: fmt.Sprintf("import-compound-%d", i+1)}
		if err := f.s.opts.Store.Get(t.Context(), key, &source); err != nil {
			t.Fatal(err)
		}
		if err := follower.Get(t.Context(), key, &target); err != nil || len(target.Spec.Endpoints) != count || source.UID != target.UID || source.ResourceVersion != target.ResourceVersion || store.ManagedBy(&target) != store.ManagerAPI {
			t.Fatal("restart lost complete chunk identity/content", key, err)
		}
	}
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "compound-import-promotion"}); err != nil {
		t.Fatal(err)
	}
	servingZoneReadyRequest(t, f, srv, http.MethodDelete, "/api/v1/zones/compound", nil, http.StatusOK)
	// A second follower proves complete replacement preserves deletions rather
	// than merging the former multi-chunk import back into an empty image.
	empty, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "replacement-follower.db")
	follower = open()
	if err := follower.Enroll(t.Context(), empty.Term); err != nil {
		t.Fatal(err)
	}
	var expected replication.Generation
	validateAndInstall := func(img replication.Image) {
		t.Helper()
		v, err := follower.ValidateImage(t.Context(), img)
		if err != nil {
			t.Fatal(err)
		}
		g, err := follower.InstallReplica(t.Context(), v, replication.InstallOptions{ExpectedTerm: img.Term, ExpectedInstalled: expected})
		if err != nil {
			t.Fatal(err)
		}
		expected = g
	}
	validateAndInstall(*image)
	validateAndInstall(*empty)
	if err := follower.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "compound"}, &z); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("replacement restored deleted zone", err)
	}
	for i := range 2 {
		var rs public.DNSRecordSet
		if err := follower.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: fmt.Sprintf("import-compound-%d", i+1)}, &rs); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("replacement restored deleted import chunk", err)
		}
	}
}
