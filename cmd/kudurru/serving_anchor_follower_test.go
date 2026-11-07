package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

type anchorFollowerFixture struct {
	s       *servingRuntime
	store   *sqlite.Store
	path    string
	options sqlite.Options
	runtime servingRuntimeOptions
}

func newAnchorFollowerFixture(ctx context.Context, t *testing.T, primary *servingRuntime) *anchorFollowerFixture {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	f := &anchorFollowerFixture{path: filepath.Join(t.TempDir(), "follower.db"), options: sqlite.Options{
		Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(),
		InitialMode: replication.Follower, Identity: "root-anchor-follower",
		ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage,
	}}
	f.store, err = sqlite.Open(f.path, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if f.s != nil {
			if err := f.s.Close(closeCtx); err != nil {
				t.Error(err)
			}
		}
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := f.store.Enroll(ctx, primary.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	// Capture the actual schema8 receiver BEFORE this original source read.
	installCtx := captureServingReplica(t, f.store)
	expected := f.store.Authority().Installed
	image, err := primary.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := f.store.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := f.store.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: expected})
	if err != nil || !installed.Same(image.Generation()) {
		t.Fatal("actual captured follower install", err)
	}
	f.runtime = primary.opts
	f.runtime.Store = f.store
	f.runtime.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
	f.s, err = newServingRuntime(ctx, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *anchorFollowerFixture) reopen(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := f.s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = sqlite.Open(f.path, f.options)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.Store = f.store
	f.s, err = newServingRuntime(ctx, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
}

func anchorFollowerImage(ctx context.Context, t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	image, err := s.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func anchorFollowerSameImage(a, b *replication.Image) bool {
	return a.Generation().Same(b.Generation()) && a.ObjectResourceVersion == b.ObjectResourceVersion &&
		reflect.DeepEqual(a.KindManifest, b.KindManifest) && reflect.DeepEqual(a.Payload, b.Payload)
}

func TestAnchorFollowerWireNormalThenDenialAndRestart(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 3, nil)
	primary, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	primaryClock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := primary.captureAnchorOperation(ctx, target, host, primaryClock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := bootstrap.run(); err != nil || receipt.Update.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("original primary bootstrap", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, ok := sqlite.WriterTokenFromContext(ctx); ok {
		t.Fatal("follower inherited a primary writer")
	}
	f := newAnchorFollowerFixture(ctx, t, primary)
	before := anchorFollowerImage(ctx, t, f.store)
	authority := f.store.Authority()
	_, token, err := f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := f.store.CaptureAnchorPublication(ctx, token)
	if err != nil || !publication.Input().Current() {
		t.Fatal("actual original follower source was not eligible", err)
	}
	// A follower uses its own original clock and its own raw socket arrival;
	// no leader receipt, observation or primary clock is supplied to it.
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	for step := 2; step <= 3; step++ {
		op, err := f.s.captureAnchorFollowerDenial(ctx, clock, 0)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := op.run()
		if step == 2 {
			if !errors.Is(err, replication.ErrInvalid) || receipt.Outcome() == sqlite.AnchorCommitted || !publication.Input().Current() {
				t.Fatal("normal follower observation advanced or retired trust", err)
			}
		} else if err != nil || receipt.Outcome() != sqlite.AnchorCommitted || publication.Input().Current() {
			t.Fatal("actual follower raw self-revoke did not retire local admission", err)
		}
		if reads.Load() != int32(step) || !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || !reflect.DeepEqual(authority, f.store.Authority()) {
			t.Fatal("follower acquisition changed canonical history, checkpoint or authority")
		}
		if _, err := f.store.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
			t.Fatal("deny-only acquisition gained a writer", err)
		}
		if _, err := op.run(); err == nil || reads.Load() != int32(step) {
			t.Fatal("follower operation reacquired or replayed")
		}
	}
	f.reopen(ctx, t)
	if !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || !reflect.DeepEqual(authority, f.store.Authority()) {
		t.Fatal("actual follower reopen changed canonical state")
	}
	_, token, err = f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CaptureAnchorPublication(ctx, token); !errors.Is(err, sqlite.ErrAnchorUnavailable) {
		t.Fatal("actual reopen forgot durable local denial", err)
	}
	if _, err := f.store.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower restart invented primary progress", err)
	}
}

func TestAnchorFollowerWireOriginalAdmission(t *testing.T) {
	for _, kind := range []string{"primary", "unbounded", "canceled", "promoted", "closed"} {
		t.Run(kind, func(t *testing.T) {
			endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
			primary, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
			clock, err := trustanchors.NewSessionClock()
			if err != nil {
				t.Fatal(err)
			}
			bootstrap, err := primary.captureAnchorOperation(ctx, target, host, clock, 0)
			if err != nil {
				t.Fatal(err)
			}
			if receipt, err := bootstrap.run(); err != nil || receipt.Update.Outcome() != sqlite.AnchorCommitted {
				t.Fatal("original primary bootstrap", err)
			}
			if kind == "primary" {
				if _, err := primary.captureAnchorFollowerDenial(ctx, clock, 0); err == nil || reads.Load() != 1 {
					t.Fatal("primary became a follower acquisition")
				}
				return
			}
			ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
			defer stop()
			f := newAnchorFollowerFixture(ctx, t, primary)
			if kind == "unbounded" {
				if _, err := f.s.captureAnchorFollowerDenial(context.Background(), clock, 0); err == nil || reads.Load() != 1 {
					t.Fatal("unbounded original follower acquisition")
				}
				return
			}
			original, cancel := context.WithCancel(ctx)
			defer cancel()
			op, err := f.s.captureAnchorFollowerDenial(original, clock, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "canceled":
				cancel()
			case "promoted":
				a := f.store.Authority()
				if _, err := f.store.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed, RequestID: "original-follower"}); err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := f.s.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			before := anchorFollowerImage(ctx, t, f.store)
			if receipt, err := op.run(); err == nil || receipt.Outcome() == sqlite.AnchorCommitted || reads.Load() != 1 {
				t.Fatal("original follower refusal acquired fresh wire", err)
			}
			if !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) {
				t.Fatal("refused follower operation changed canonical data")
			}
		})
	}
}

func TestAnchorFollowerWireKnownRevokeAfterPromotionClosesAdmission(t *testing.T) {
	type promotedSource struct {
		image *replication.Image
		input *sqlite.AnchorResolverInput
		err   error
	}
	interpose := make(chan func() promotedSource, 1)
	observed := make(chan promotedSource, 1)
	endpoint, seeds, reads := anchorOperationServer(t, 2, func(n int) {
		if n == 2 {
			observed <- (<-interpose)()
		}
	})
	primary, original, target, host := anchorOperationFixture(t, endpoint, seeds)
	primaryClock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := primary.captureAnchorOperation(original, target, host, primaryClock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := bootstrap.run(); err != nil || receipt.Update.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("original primary bootstrap", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f := newAnchorFollowerFixture(ctx, t, primary)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.s.captureAnchorFollowerDenial(ctx, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	interpose <- func() promotedSource {
		a := f.store.Authority()
		if _, err := f.store.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed, RequestID: "in-flight-follower"}); err != nil {
			return promotedSource{err: err}
		}
		image, token, err := f.store.ReadServingCandidate(ctx)
		if err != nil {
			return promotedSource{err: err}
		}
		publication, err := f.store.CaptureAnchorPublication(ctx, token)
		if err != nil {
			return promotedSource{err: err}
		}
		if !publication.Input().Current() {
			return promotedSource{err: errors.New("promoted source was already unavailable")}
		}
		return promotedSource{image: image, input: publication.Input()}
	}
	receipt, runErr := op.run()
	var promoted promotedSource
	select {
	case promoted = <-observed:
	case <-ctx.Done():
		t.Fatal("original acquisition did not reach actual promotion")
	}
	if promoted.err != nil {
		t.Fatal("promotion fixture", promoted.err)
	}
	if runErr == nil || receipt.Outcome() == sqlite.AnchorCommitted || reads.Load() != 2 || promoted.input.Current() {
		t.Fatal("known old-follower revoke was suppressed or adopted promoted authority", runErr)
	}
	if !anchorFollowerSameImage(promoted.image, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("refused old follower denial changed promoted canonical history")
	}
	_, token, err := f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CaptureAnchorPublication(ctx, token); !errors.Is(err, sqlite.ErrAnchorUnavailable) {
		t.Fatal("unknown/refused known denial reopened promoted local admission", err)
	}
	if _, err := op.run(); err == nil || reads.Load() != 2 {
		t.Fatal("old follower operation reacquired under promoted authority")
	}
}
