package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Readiness precedes the first operation capture and raw acquisition. Only an
// absent first image or an actual awaiting/dormant owner may wait. Once a poller
// starts, no source wake can reinterpret its refusal as startup readiness. A
// fenced or conflicted member is classified from its own DNS-only cut (D369).
func (s *servingRuntime) anchorFollowerReady(ctx context.Context, epoch anchorFollowerEpoch) (bool, error) {
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	image, token, err := s.opts.Store.ReadServingCandidate(original)
	if err != nil {
		a, blocked := s.opts.Store.ServingAuthority()
		if errors.Is(err, replication.ErrReadOnly) && !blocked && a.Mode == replication.Follower && epoch.same(followerEpoch(a)) && a.Installed == (replication.Generation{}) {
			return false, original.Err()
		}
		return false, err
	}
	if image == nil || !image.Generation().Same(token.Generation()) || !supervisedAnchorRole(token.Authority()) || !epoch.same(followerEpoch(token.Authority())) {
		return false, replication.ErrFenced
	}
	ready := false
	if !accepted.IsAwaitingOwner(image.Payload.Runtime) {
		owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			return false, err
		}
		settings, err := anchorImageSettings(image)
		if err != nil {
			return false, err
		}
		document, err := servingSettingsDocument(settings)
		if err != nil {
			return false, err
		}
		effective, _, err := config.ApplyClusterDocument(s.opts.Config, document)
		if err != nil {
			return false, err
		}
		policy, err := effective.Upstreams.Recursion.RootAnchorPolicy()
		if err != nil {
			return false, err
		}
		root := owner.Inspect()
		switch root.Phase {
		case accepted.Dormant:
			// Static policy needs no poll; managed policy must await the
			// primary's committed bootstrap. Neither initializes local trust.
		case accepted.Managed:
			if policy.Mode != config.RootAnchorsManaged || root.PolicyID != policy.PolicyID || root.SeedDigest != policy.SeedDigest {
				return false, errAnchorPolicy
			}
			ready = true
		default:
			return false, errAnchorPolicy
		}
	}
	// This read-only classification belongs to the same original installed cut.
	// It grants no publication, receipt, writer or operation continuation. The
	// check carries the token's own disposition, never a writable one.
	if err := s.opts.Store.PublishIfCurrent(original, token, sqlite.PublicationOptions{DNSOnly: token.DNSOnly()}, func() error { return nil }); err != nil {
		return false, err
	}
	return ready, nil
}

type anchorFollowerChild struct {
	cancel context.CancelFunc
	done   chan error
}

func (s *servingRuntime) startAnchorFollower(ctx context.Context, epoch anchorFollowerEpoch) *anchorFollowerChild {
	ctx, cancel := context.WithCancel(ctx)
	child := &anchorFollowerChild{cancel: cancel, done: make(chan error, 1)}
	go func() { child.done <- s.runAnchorFollowerEpoch(ctx, epoch) }()
	return child
}

// Run mounts this once for the admitted runtime. Its terminal record survives
// ordinary generation/installed-image changes, including their incarnation
// increments. Only a real member, term or role change admits another child,
// after joining the old one. Failure parks this worker without taking unrelated
// serving workers down. Every role without an elected updater is supervised:
// a fenced or conflicted member keeps serving last-good DNS, so it keeps its
// own deny-only polling too (D369). A role change inside one term is a durable
// authority transition (fenced to follower by install, follower or fenced to
// conflict or divergence), not an ordinary install, and each can happen only a
// bounded number of times per term.
func (s *servingRuntime) superviseAnchorFollower(ctx context.Context) error {
	var epoch anchorFollowerEpoch
	var terminal bool
	var nextRead time.Time
	var child *anchorFollowerChild
	join := func() {
		if child != nil {
			child.cancel()
			if err := <-child.done; err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("joined root anchor follower stopped", "err", err)
			}
			child = nil
		}
	}
	defer join()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		changed, authority := s.opts.Store.GenerationChanges(), s.opts.Store.AuthorityChanges()
		a, blocked := s.opts.Store.ServingAuthority()
		next := followerEpoch(a)
		if !epoch.sameRole(next) {
			join()
			epoch, terminal = next, false
			nextRead = time.Time{}
		} else if !terminal && child == nil {
			// Before the first poll, readiness may observe a newly installed
			// image. After capture, that same change cannot rearm a refusal.
			epoch = next
		}
		if blocked || !supervisedAnchorRole(a) || !s.anchors.admission.current() {
			if child != nil {
				join()
				terminal = true
			}
		} else if !terminal && child == nil && !time.Now().Before(nextRead) {
			ready, err := s.anchorFollowerReady(ctx, epoch)
			nextRead = time.Now().Add(time.Second)
			if err != nil {
				terminal = true
				slog.Error("root anchor follower stopped for this authority epoch", "err", err)
			} else if ready {
				child = s.startAnchorFollower(ctx, epoch)
			}
		}
		var done <-chan error
		if child != nil {
			done = child.done
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			child.cancel()
			child, terminal = nil, true
			if err != nil {
				slog.Error("root anchor follower stopped for this authority epoch", "err", err)
			}
		case <-changed:
		case <-authority:
		case <-timer.C:
		}
	}
}
