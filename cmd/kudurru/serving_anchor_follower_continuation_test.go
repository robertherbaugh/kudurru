package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

func anchorFollowerContinuationFixture(t *testing.T, endpoint string, seeds []string) (*anchorFollowerFixture, context.Context, *anchorFollowerPoller, func() *replication.Image) {
	t.Helper()
	primary, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	advance := func() {
		op, err := primary.captureAnchorOperation(ctx, target, host, clock, 0)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := op.run(); err != nil || result.Update.Outcome() != sqlite.AnchorCommitted {
			t.Fatal("original primary observation", err)
		}
	}
	advance()
	f := newAnchorFollowerFixture(ctx, t, primary)
	followerClock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	u := &anchorFollowerPoller{runtime: f.s, clock: followerClock}
	return f, ctx, u, func() *replication.Image {
		advance()
		return installAnchorFollower(ctx, t, f, primary)
	}
}

func TestAnchorFollowerCompletedNormalPermitsFreshInstalledCapture(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(_ int32, _ *dns.Msg) {})
	f, ctx, poller, install := anchorFollowerContinuationFixture(t, endpoint, seeds)
	first, err := poller.step(ctx)
	if err != nil || !first.attempted || first.result.branch != sqlite.AnchorPollNormal || reads.Load() != 2 {
		t.Fatal("original normal follower receipt", err, reads.Load())
	}
	oldEpoch := poller.epoch
	installed := install()
	if oldEpoch.same(followerEpoch(f.store.Authority())) {
		t.Fatal("fixture did not cross actual installed incarnation")
	}
	second, err := poller.step(ctx)
	if err != nil || !second.attempted || second.result.branch != sqlite.AnchorPollNormal || reads.Load() != 4 {
		t.Fatal("completed normal receipt prevented a new original installed acquisition", err, reads.Load())
	}
	if !anchorFollowerSameImage(installed, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("fresh follower observation advanced canonical state")
	}
	held, err := poller.step(ctx)
	if err != nil || held.attempted || !held.next.Equal(second.next) || reads.Load() != 4 {
		t.Fatal("new source lost its normal refresh cadence", err, reads.Load())
	}
}

func TestAnchorFollowerHeldReceiptCannotAdoptNewInstall(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	endpoint, seeds, reads := anchorSupervisorServer(t, func(n int32, _ *dns.Msg) {
		if n == 4 {
			close(entered)
			<-release
		}
	})
	f, ctx, poller, install := anchorFollowerContinuationFixture(t, endpoint, seeds)
	if first, err := poller.step(ctx); err != nil || first.result.branch != sqlite.AnchorPollNormal || reads.Load() != 2 {
		t.Fatal("initial normal follower observation", err)
	}
	install() // a completed normal operation permits the next fresh capture
	done := make(chan error, 1)
	go func() { _, err := poller.step(ctx); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("follower did not capture original held receipt")
	}
	installed := install()
	unblock()
	var originalErr error
	select {
	case originalErr = <-done:
	case <-ctx.Done():
		t.Fatal("held follower operation did not join")
	}
	var transport *anchorAcquisitionFailure
	if originalErr == nil || errors.As(originalErr, &transport) || poller.normalCompleted || reads.Load() != 5 {
		t.Fatal("held original receipt adopted installed successor", originalErr, reads.Load())
	}
	if _, err := poller.step(ctx); err != originalErr || reads.Load() != 5 {
		t.Fatal("refused held operation was recaptured", err, reads.Load())
	}
	if !anchorFollowerSameImage(installed, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("refused follower changed installed canonical data")
	}
}
