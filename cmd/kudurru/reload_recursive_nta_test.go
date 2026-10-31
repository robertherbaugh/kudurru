package main

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestRecursiveNTAReloadAndSettingsRestore(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal("default config did not load")
	}
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: "recursive"}}
	rec, err := recursor.New(upstream.RecursionFrom(cfg.Upstreams.Recursion), recursor.Options{})
	if err != nil {
		t.Fatal("startup resolver construction failed")
	}
	opts := upstream.Options{Recursor: rec}
	expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	cfg.Upstreams.Recursion.NegativeTrustAnchors = []config.RecursiveNegativeTrustAnchor{{Domain: "broken.example.", Reason: "incident", ExpiresAt: expiry}}
	restored, err := config.FromFlat(config.Flatten(cfg))
	if err != nil {
		t.Fatal("persisted recursive settings failed to restore")
	}
	changed := []string{"upstreams.recursion.negativeTrustAnchors"}
	set, err := buildUpstreams(restored, changed, opts)
	if err != nil {
		t.Fatal("production recursive NTA reload failed")
	}
	defer func() { _ = set.Close() }()
	active, until := rec.TrustContext()
	if set.Recursor() != rec || !until.Equal(expiry) {
		t.Fatal("reload failed to apply original typed expiry")
	}
	// Invalid replacement must leave existing admission and deadline intact.
	bad := *restored
	bad.Upstreams.Recursion.NegativeTrustAnchors = []config.RecursiveNegativeTrustAnchor{{Domain: "broken.example.", ExpiresAt: expiry}}
	if _, err := buildUpstreams(&bad, changed, opts); err == nil {
		t.Fatal("invalid recursive exception was accepted")
	}
	still, until := rec.TrustContext()
	if active != still || !until.Equal(expiry) {
		t.Fatal("failed reload changed recursive authority")
	}
	// Removal retires old native key/delegation/cache context, without renewal.
	removed := *restored
	removed.Upstreams.Recursion.NegativeTrustAnchors = nil
	replacement, err := buildUpstreams(&removed, changed, opts)
	if err != nil {
		t.Fatal("recursive exception removal failed")
	}
	defer func() { _ = replacement.Close() }()
	without, until := rec.TrustContext()
	if without == active || !until.IsZero() {
		t.Fatal("removal retained exception authority")
	}
}
