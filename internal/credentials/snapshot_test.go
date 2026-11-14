package credentials

import (
	"bytes"
	"context"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestSnapshotPreservesOriginalCapabilityAndFailure(t *testing.T) {
	value := "first-private-value"
	resolver, err := New(Options{Environment: []string{"DELEGATED", "MISSING"}, Getenv: func(name string) (string, bool) { return value, name == "DELEGATED" }, FingerprintKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	ref := public.CredentialReference{Env: "DELEGATED"}
	original, err := resolver.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := resolver.Snapshot(t.Context(), []public.CredentialReference{ref, {Env: "MISSING"}})
	if err != nil {
		t.Fatal(err)
	}
	value = "replacement-private-value"
	first, err := captured.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes, original.Bytes) || first.Fingerprint != original.Fingerprint {
		t.Fatal("snapshot substituted replacement authority")
	}
	clear(first.Bytes)
	second, err := captured.Resolve(t.Context(), ref)
	if err != nil || !bytes.Equal(second.Bytes, original.Bytes) {
		t.Fatal("snapshot shares mutable bytes")
	}
	if _, err = captured.Resolve(t.Context(), public.CredentialReference{Env: "MISSING"}); err == nil {
		t.Fatal("snapshot repaired original failure")
	}
	if _, err = captured.Resolve(t.Context(), public.CredentialReference{Env: "OTHER"}); err != ErrDenied {
		t.Fatal("snapshot broadened capability")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = captured.Resolve(ctx, ref); err != context.Canceled {
		t.Fatal("snapshot ignored original cancellation")
	}
}
