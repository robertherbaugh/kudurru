package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	stored "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

func runtimeTSIGSealer(t *testing.T, s *servingRuntime) *tsigkeys.Sealer {
	t.Helper()
	cut := s.opts.AuthKeys.owner.Capture()
	seal, err := tsigkeys.NewSealer(cut.Value().current, cut.Value().previous)
	if err != nil {
		t.Fatal("prepare runtime encryption fixture")
	}
	return seal
}

func runtimeTSIGMaterial(t *testing.T, s *servingRuntime, name, uid string, secret byte) *tsigkeys.Material {
	t.Helper()
	material, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: s.opts.Store.Info().ClusterID, UID: uid, KeyName: name, Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{secret}, 32))
	if err != nil {
		t.Fatal("prepare runtime stored credential fixture")
	}
	return material
}

func runtimeTSIGApply(t *testing.T, s *servingRuntime, seal *tsigkeys.Sealer, material *tsigkeys.Material, rv string) *stored.TSIGKey {
	t.Helper()
	row, err := seal.Seal(material)
	if err != nil {
		t.Fatal("seal runtime credential fixture")
	}
	value, err := s.opts.Store.Apply(t.Context(), row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: rv == "", ExpectedResourceVersion: rv})
	if err != nil {
		t.Fatal("persist runtime ciphertext fixture")
	}
	return value.(*stored.TSIGKey)
}

func TestTSIGRuntimeCompleteReadOnlySourceAndFailedPreparation(t *testing.T) {
	s := newServingRuntimeFixture(t)
	baseline := s.opts.Keyrings.Capture().Keys()
	r, err := newTSIGRuntime(s.opts.Store, s.opts.AuthKeys, s.opts.Keyrings, baseline)
	if err != nil {
		t.Fatal(err)
	}
	seal := runtimeTSIGSealer(t, s)
	material := runtimeTSIGMaterial(t, s, "stored.example.", "stored-incarnation", 0x31)
	row := runtimeTSIGApply(t, s, seal, material, "")
	before, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal("complete source did not prepare", err)
	}
	original := s.opts.Keyrings.Capture()
	identity, ok := original.Keys().Identity("stored.example.")
	if !ok || original.Prepared() == nil || len(original.Keys().Names()) != 2 || original.Prepared().Matches(t.Context(), before) != nil {
		t.Fatal("source failed to retain exact encrypted inventory and file baseline")
	}
	after, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("read-only preparation changed durable source")
	}
	if err := r.prepare(t.Context()); err != nil || !original.Current() {
		t.Fatal("unchanged source installed a false replacement")
	}

	// A same-name file/store conflict rejects the WHOLE source, retaining the
	// coherent old provider. It must not silently omit the conflicting record.
	duplicate := runtimeTSIGMaterial(t, s, "updates.", "duplicate-incarnation", 0x32)
	duplicateRow := runtimeTSIGApply(t, s, seal, duplicate, "")
	if err := r.prepare(t.Context()); err == nil || !original.Current() {
		t.Fatal("duplicate input installed a partial provider")
	}
	conflicted, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil || original.Prepared().Matches(t.Context(), conflicted) == nil {
		t.Fatal("old source certified a successor conflicted image")
	}
	if err := s.opts.Store.Delete(t.Context(), store.KeyOf(duplicateRow), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}

	// Well-formed ciphertext under an unavailable encryption key also rejects
	// the whole candidate, without borrowing the old key or a file fallback.
	foreignSeal, err := tsigkeys.NewSealer(bytes.Repeat([]byte{0x44}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	row = runtimeTSIGApply(t, s, foreignSeal, material, row.ResourceVersion)
	if err := r.prepare(t.Context()); err == nil || !original.Current() {
		t.Fatal("unavailable encryption silently replaced or omitted stored keys")
	}
	runtimeTSIGApply(t, s, seal, material, row.ResourceVersion)
	if err := r.prepare(t.Context()); err != nil || original.Current() {
		t.Fatal("repaired exact source was not installed")
	}
	currentIdentity, ok := s.opts.Keyrings.Capture().Keys().Identity("stored.example.")
	if !ok || currentIdentity != identity {
		t.Fatal("resealing changed the original credential incarnation")
	}

	// Encryption rotation reopens with the captured previous key, retains the
	// credential epoch, and changes the encryption installation used by serving.
	oldKey := bytes.Clone(s.opts.AuthKeys.owner.Capture().Value().current)
	previous := s.opts.Keyrings.Capture()
	if err := s.opts.AuthKeys.replace(t.Context(), keys.Keys{Current: bytes.Repeat([]byte{0x55}, 32), Previous: oldKey}); err != nil {
		t.Fatal(err)
	}
	if err := r.prepare(t.Context()); err != nil || previous.Current() {
		t.Fatal("encryption replacement did not install a new prepared owner")
	}
	current := s.opts.Keyrings.Capture()
	rotatedIdentity, ok := current.Keys().Identity("stored.example.")
	if !ok || rotatedIdentity != identity || current.Prepared().EncryptionIdentity() == previous.Prepared().EncryptionIdentity() {
		t.Fatal("encryption rotation lost epoch or retained stale encryption owner")
	}
}

func TestTSIGRuntimeIndependentSourceChangesAndJoinedCancellation(t *testing.T) {
	s := newServingRuntimeFixture(t)
	r, err := newTSIGRuntime(s.opts.Store, s.opts.AuthKeys, s.opts.Keyrings, s.opts.Keyrings.Capture().Keys())
	if err != nil {
		t.Fatal(err)
	}
	seal := runtimeTSIGSealer(t, s)
	first := runtimeTSIGMaterial(t, s, "stored.example.", "source-incarnation", 0x61)
	row := runtimeTSIGApply(t, s, seal, first, "")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error("read-only TSIG worker failed to join", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("read-only TSIG worker did not join")
		}
	}
	joined := false
	defer func() {
		if !joined {
			stop()
		}
	}()
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatal("read-only TSIG source did not converge")
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(func() bool { return s.opts.Keyrings.Capture().Prepared() != nil })
	original := s.opts.Keyrings.Capture()
	originalID, _ := original.Keys().Identity("stored.example.")
	second := runtimeTSIGMaterial(t, s, "stored.example.", "source-incarnation", 0x62)
	row = runtimeTSIGApply(t, s, seal, second, row.ResourceVersion)
	wait(func() bool { return !original.Current() })
	middle := s.opts.Keyrings.Capture()
	middleID, _ := middle.Keys().Identity("stored.example.")
	third := runtimeTSIGMaterial(t, s, "stored.example.", "source-incarnation", 0x61)
	runtimeTSIGApply(t, s, seal, third, row.ResourceVersion)
	wait(func() bool { return !middle.Current() })
	finalID, _ := s.opts.Keyrings.Capture().Keys().Identity("stored.example.")
	if original.Current() || originalID == finalID || originalID == middleID {
		t.Fatal("A-B-A source bytes revived an original stored credential epoch")
	}
	stop()
	joined = true
	last := s.opts.Keyrings.Capture()
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) || !last.Current() {
		t.Fatal("canceled lifetime restarted source preparation")
	}
}
