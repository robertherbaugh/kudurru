package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

func TestTSIGAcquisitionOriginalSourceAcrossUnavailableCoordinator(t *testing.T) {
	s := newServingRuntimeFixture(t)
	r, err := newTSIGRuntime(s.opts.Store, s.opts.AuthKeys, s.opts.Keyrings, s.opts.Keyrings.Capture().Keys())
	if err != nil {
		t.Fatal(err)
	}
	seal := runtimeTSIGSealer(t, s)
	material := runtimeTSIGMaterial(t, s, "stored.example.", "acquisition-owner", 0x71)
	row := runtimeTSIGApply(t, s, seal, material, "")
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	original := r.captureAcquisition()
	if original == nil {
		t.Fatal("complete source was not captured")
	}
	entered := false
	if err := original.WithCurrent(t.Context(), func() error { entered = true; return nil }, nil); err != nil || !entered {
		t.Fatal("original source could not enter local acceptance", err)
	}

	// Store changes without installing a successor ring. Ring currency alone
	// remains true, but the original durable source cannot certify acceptance.
	replacement := runtimeTSIGMaterial(t, s, "stored.example.", "acquisition-owner", 0x72)
	runtimeTSIGApply(t, s, seal, replacement, row.ResourceVersion)
	entered = false
	if !original.Current() {
		t.Fatal("fixture did not retain the installed old ring")
	}
	if err := original.WithCurrent(t.Context(), func() error { entered = true; return nil }, nil); !errors.Is(err, tsigkeys.ErrSource) || entered {
		t.Fatal("coordinator lag admitted a changed durable source", err)
	}
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deps, err := s.captureHost(t.Context(), image)
	if deps.Release != nil {
		deps.Release()
	}
	if err == nil {
		t.Fatal("complete publication adopted the retained old source")
	}
}

func TestTSIGAcquisitionLocalCacheRaceIsNotAtomicServingAdmission(t *testing.T) {
	s := newServingRuntimeFixture(t)
	r, err := newTSIGRuntime(s.opts.Store, s.opts.AuthKeys, s.opts.Keyrings, s.opts.Keyrings.Capture().Keys())
	if err != nil {
		t.Fatal(err)
	}
	seal := runtimeTSIGSealer(t, s)
	material := runtimeTSIGMaterial(t, s, "stored.example.", "local-cache-owner", 0x73)
	row := runtimeTSIGApply(t, s, seal, material, "")
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	original := r.captureAcquisition()
	if original == nil {
		t.Fatal("complete source was not captured")
	}
	fingerprint, ok := dnsserver.AcquisitionFingerprint(original.Keys(), "stored.example.")
	if !ok {
		t.Fatal("original acquired provenance was unavailable")
	}
	local, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	published := false
	err = original.WithCurrent(t.Context(), func() error {
		// This is an actual distinct local.db commit, deliberately preceding
		// an authoritative source change while the memory key pin is held.
		if err := local.PutBlob(t.Context(), "acquisition-provenance", []byte(fingerprint)); err != nil {
			return err
		}
		replacement := runtimeTSIGMaterial(t, s, "stored.example.", "local-cache-owner", 0x73)
		runtimeTSIGApply(t, s, seal, replacement, row.ResourceVersion)
		return nil
	}, func() error { published = true; return nil })
	if !errors.Is(err, tsigkeys.ErrSource) || published {
		t.Fatal("raced local commit claimed publication/acceptance", err)
	}
	saved, err := local.GetBlob(t.Context(), "acquisition-provenance")
	if err != nil || string(saved.Data) != fingerprint {
		t.Fatal("fixture lost explicit original cache provenance", err)
	}
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := r.captureAcquisition()
	if next == nil {
		t.Fatal("successor source was unavailable")
	}
	freshFingerprint, ok := dnsserver.AcquisitionFingerprint(next.Keys(), "stored.example.")
	if !ok || freshFingerprint == fingerprint || original.Current() {
		t.Fatal("same bytes/new epoch relabeled an original acquired artifact")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	entered := false
	if err := next.WithCurrent(canceled, func() error { entered = true; return nil }, nil); !errors.Is(err, context.Canceled) || entered {
		t.Fatal("canceled local acquisition entered acceptance", err)
	}
}

func TestTSIGAcquisitionNoFallibleSourceReadAfterPublication(t *testing.T) {
	s := newServingRuntimeFixture(t)
	r, err := newTSIGRuntime(s.opts.Store, s.opts.AuthKeys, s.opts.Keyrings, s.opts.Keyrings.Capture().Keys())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	original := r.captureAcquisition()
	if original == nil {
		t.Fatal("complete empty stored source was unavailable")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	published := false
	err = original.WithCurrent(ctx, func() error { return nil }, func() error {
		// Model a successful atomic memory swap immediately followed by time
		// cancellation. This callback does no storage/reference/network work.
		if err := ctx.Err(); err != nil {
			return err
		}
		published = true
		cancel()
		return nil
	})
	if err != nil || !published {
		t.Fatal("successful memory swap was followed by fallible source/time work", err)
	}

	// A cancellation during local cache work must instead fail BEFORE the
	// publication callback; a local artifact is not an accepted job receipt.
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	published = false
	err = original.WithCurrent(ctx, func() error { cancel(); return nil }, func() error { published = true; return nil })
	if !errors.Is(err, context.Canceled) || published {
		t.Fatal("canceled local phase exposed a manager publication", err)
	}
}
