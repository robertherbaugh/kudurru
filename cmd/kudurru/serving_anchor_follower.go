package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// anchorFollowerDenial is a single original local acquisition. It has no
// primary writer, normal update, bootstrap or continuation. The eventual
// follower worker must create its finite lifetime before capture; it cannot
// feed leader-received, held or forwarded bytes into this socket-only adapter.
type anchorFollowerDenial struct {
	runtime  *servingRuntime
	ctx      context.Context
	token    sqlite.GenerationToken
	wire     *anchorWirePlan
	denial   *sqlite.AnchorDenial
	owner    accepted.Snapshot
	identity anchorRetrySource
	used     atomic.Bool
}

func (s *servingRuntime) captureAnchorFollowerDenial(ctx context.Context, clock *trustanchors.SessionClock, offset uint64) (*anchorFollowerDenial, error) {
	if s == nil || s.opts.Store == nil || s.anchors == nil || s.life == nil || ctx == nil || clock == nil {
		return nil, errAnchorPolicy
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, finite := ctx.Deadline()
	if !finite || time.Until(deadline) > 30*time.Second || s.life.Err() != nil {
		return nil, errAnchorPolicy
	}
	image, token, err := s.opts.Store.ReadServingCandidate(ctx)
	if err != nil {
		return nil, err
	}
	if image == nil || !image.Generation().Same(token.Generation()) || token.Authority().Mode != replication.Follower || token.DNSOnly() {
		return nil, errAnchorPolicy
	}
	settings, err := anchorImageSettings(image)
	if err != nil {
		return nil, err
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return nil, err
	}
	effective, _, err := config.ApplyClusterDocument(s.opts.Config, document)
	if err != nil {
		return nil, err
	}
	policy, err := effective.Upstreams.Recursion.RootAnchorPolicy()
	if err != nil {
		return nil, err
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		return nil, err
	}
	root := owner.Inspect()
	if policy.Mode != config.RootAnchorsManaged || root.Phase != accepted.Managed || root.PolicyID != policy.PolicyID || root.SeedDigest != policy.SeedDigest {
		return nil, errAnchorPolicy
	}
	endpoints, err := anchorPolicyEndpoints(effective.Upstreams.Recursion)
	if err != nil {
		return nil, err
	}
	wire, err := captureAnchorWirePlan(ctx, endpoints, "tcp", offset)
	if err != nil {
		return nil, err
	}
	denial, err := s.opts.Store.CaptureAnchorDenial(ctx, owner.Version(), clock)
	if err != nil {
		return nil, err
	}
	op := &anchorFollowerDenial{runtime: s, ctx: ctx, token: token, wire: wire, denial: denial, owner: owner,
		identity: anchorRetrySource{owner: owner.Version(), policyID: policy.PolicyID, seeds: policy.SeedDigest,
			settingsUID: string(settings.UID), settingsVersion: settings.ResourceVersion}}
	// Close image-read -> local driver capture interposition with an inert
	// callback. No source I/O or additional pin is acquired under Store wmu.
	if err := s.opts.Store.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error { return nil }); err != nil {
		return nil, err
	}
	if err := op.checkOriginal(); err != nil {
		return nil, err
	}
	return op, nil
}

func (op *anchorFollowerDenial) checkOriginal() error {
	if op == nil || op.runtime == nil || op.ctx == nil || op.runtime.life == nil {
		return errAnchorPolicy
	}
	if err := op.ctx.Err(); err != nil {
		return err
	}
	if op.runtime.life.Err() != nil {
		return errAnchorPolicy
	}
	authority, blocked := op.runtime.opts.Store.ServingAuthority()
	if blocked || authority.Mode != replication.Follower || !op.token.Matches(authority) {
		return replication.ErrFenced
	}
	return nil
}

func (op *anchorFollowerDenial) run() (sqlite.AnchorCommit, error) {
	result, err := op.observe()
	// Preserve the original one-shot denial contract. The poller consumes the
	// explicit authenticated normal branch through observe instead.
	if err == nil && result.branch == sqlite.AnchorPollNormal {
		err = replication.ErrInvalid
	}
	return result.completion, err
}

type anchorFollowerCompletion struct {
	branch     sqlite.AnchorPollBranch
	completion sqlite.AnchorCommit
}

func (op *anchorFollowerDenial) observe() (result anchorFollowerCompletion, err error) {
	if op == nil || !op.used.CompareAndSwap(false, true) {
		return result, errAnchorPolicy
	}
	if err := op.checkOriginal(); err != nil {
		return result, err
	}
	arrived := false
	raw, stamp, err := acquireAnchorWire(op.wire, func() (sqlite.AnchorReceiptStamp, error) {
		arrived = true
		return op.denial.SampleReceipt()
	})
	if err != nil {
		if !arrived {
			if refusal := op.checkOriginal(); refusal != nil {
				return result, refusal
			}
			return result, &anchorAcquisitionFailure{err: err}
		}
		return result, err
	}
	branch, verified, err := op.denial.ObserveReceived(raw, stamp)
	if err != nil {
		return result, err
	}
	result.branch = branch
	if branch == sqlite.AnchorPollNormal {
		return result, op.checkOriginal()
	}
	// Do not suppress known authenticated denial with a late granting-policy
	// check. The driver enforces the original local/authority/installed cut;
	// failed or unknown persistence closes admission and preserves disposition.
	result.completion, _, err = op.runtime.opts.Store.CommitAnchorDenial(verified)
	return result, err
}
