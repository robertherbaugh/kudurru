package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestServingRuntimeFollowerFirstImageRecovery(t *testing.T) {
	primary := newServingRuntimeFixture(t)
	image, err := primary.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []bool{false, true} {
		name := "wait-for-first-install"
		if malformed {
			name = "reject-malformed-installed-image"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "follower.db")
			storeOpts := sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator,
				InternalKinds: privateStoreKinds(), InitialMode: replication.Follower,
				Identity: "first-image-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry,
				ValidateReplicaImage: runtimeowners.ValidateImage}
			if malformed {
				// Simulate an older installation which did not validate complete
				// owner inventories. Current startup must still reject its image.
				storeOpts.ValidateReplicaImage = nil
			}
			peer, err := sqlite.Open(path, storeOpts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = peer.Close() })
			if err := peer.Enroll(t.Context(), image.Term); err != nil {
				t.Fatal(err)
			}
			install := func(candidate replication.Image) replication.Generation {
				t.Helper()
				validated, err := peer.ValidateImage(t.Context(), candidate)
				if err != nil {
					t.Fatal(err)
				}
				installed, err := peer.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
				if err != nil {
					t.Fatal(err)
				}
				return installed
			}
			if malformed {
				bad := *image
				bad.Payload.Security = slices.DeleteFunc(slices.Clone(image.Payload.Security), func(row replication.Entry) bool { return row.Key == servingauth.RootKey })
				if err := replication.Seal(&bad); err != nil {
					t.Fatal(err)
				}
				install(bad)
				if err := peer.Close(); err != nil {
					t.Fatal(err)
				}
				storeOpts.ValidateReplicaImage = runtimeowners.ValidateImage
				peer, err = sqlite.Open(path, storeOpts)
				if err != nil {
					t.Fatal(err)
				}
			}
			opts := primary.opts
			opts.Store = peer
			s, err := newServingRuntime(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			before := peer.Authority()
			if malformed {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := s.Run(ctx); !errors.Is(err, servingauth.ErrManifest) {
					t.Fatal("malformed installed auth inventory was treated as pending", err)
				}
				if !reflect.DeepEqual(before, peer.Authority()) {
					t.Fatal("failed observation changed durable authority")
				}
				return
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("pending follower failed to join")
				}
			})
			awaitServingRuntime(t, s, func() bool { return s.publisher.Status().Reason == "no_installed_image" })
			// Cross the host observer's poll interval before installing, proving
			// the same single-use Run remains alive while no owners can be served.
			select {
			case err := <-done:
				done <- err
				t.Fatal("recoverable initial follower exited", err)
			case <-time.After(1100 * time.Millisecond):
			}
			if lease, err := s.publisher.CaptureHTTP(t.Context()); err == nil {
				lease.Release()
				t.Fatal("follower authenticated without its first image")
			}
			if _, err := peer.ReadGeneration(t.Context()); !errors.Is(err, replication.ErrReadOnly) || !reflect.DeepEqual(before, peer.Authority()) {
				t.Fatal("pending follower initialized durable state", err)
			}
			installed := install(*image)
			awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
			lease, err := s.publisher.CaptureHTTP(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if !lease.Bundle().Identity().Base.Same(installed) {
				t.Fatal("first publication substituted a different replica")
			}
			after, err := peer.ReadGeneration(t.Context())
			if err != nil || !after.Generation().Same(installed) {
				t.Fatal("follower recovery wrote durable state", err)
			}
		})
	}
}
