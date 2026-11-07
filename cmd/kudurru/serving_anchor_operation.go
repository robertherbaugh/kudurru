package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

var errAnchorPolicy = errors.New("root anchor policy unavailable or changed")

// anchorOperation retains every input before acquisition. A fresh current
// dependency pin excludes replacement during commit but never replaces the
// original episode host, canonical policy, context, owner, clock or writer.
type anchorOperation struct {
	runtime   *servingRuntime
	ctx       context.Context
	target    serving.TermActivation
	host      providercoordinator.HostCapture // borrowed until the episode joins
	clock     *trustanchors.SessionClock
	token     sqlite.GenerationToken
	settings  *public.Kudurru
	policy    config.RootAnchorPolicy
	owner     accepted.Snapshot
	original  persistence.Original
	wire      *anchorWirePlan
	poll      *sqlite.AnchorPoll
	bootstrap *sqlite.AnchorUpdate
	used      atomic.Bool
}

// anchorCompletion keeps actual independent effects even if a later pin,
// callback, completion check or publication acknowledgment refuses.
type anchorCompletion struct {
	Denial sqlite.AnchorCommit
	Update sqlite.AnchorCommit
}

// captureAnchorOperation does not start a timeout. The episode must create its
// original <=30s operation BEFORE this first image read and retain it throughout.
// Nil operation with nil error means never-managed static policy, no poll due.
func (s *servingRuntime) captureAnchorOperation(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture, clock *trustanchors.SessionClock, offset uint64) (*anchorOperation, error) {
	if s == nil || s.anchorPin == nil || ctx == nil || clock == nil || host.Current == nil {
		return nil, errAnchorPolicy
	}
	op := &anchorOperation{runtime: s, ctx: ctx, target: target, host: host, clock: clock}
	if err := op.checkOriginal(); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 30*time.Second {
		return nil, errAnchorPolicy
	}
	image, token, err := s.opts.Store.ReadServingCandidate(ctx)
	if err != nil {
		return nil, err
	}
	if image == nil || !image.Generation().Same(token.Generation()) {
		return nil, errAnchorPolicy
	}
	op.token = token
	op.settings, err = anchorImageSettings(image)
	if err != nil {
		return nil, err
	}
	document, err := servingSettingsDocument(op.settings)
	if err != nil {
		return nil, errAnchorPolicy
	}
	effective, _, err := config.ApplyClusterDocument(s.opts.Config, document)
	if err != nil {
		return nil, errAnchorPolicy
	}
	op.policy, err = effective.Upstreams.Recursion.RootAnchorPolicy()
	if err != nil {
		return nil, err
	}
	op.owner, err = accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		return nil, err
	}
	root := op.owner.Inspect()
	if op.policy.Mode != config.RootAnchorsManaged {
		if root.Phase == accepted.Managed {
			return nil, errAnchorPolicy
		}
		return nil, nil
	}
	if root.Phase == accepted.Managed && (root.PolicyID != op.policy.PolicyID || root.SeedDigest != op.policy.SeedDigest) {
		return nil, errAnchorPolicy
	}
	endpoints, err := anchorPolicyEndpoints(effective.Upstreams.Recursion)
	if err != nil {
		return nil, err
	}
	op.wire, err = captureAnchorWirePlan(ctx, endpoints, "tcp", offset)
	if err != nil {
		return nil, err
	}
	op.original, err = persistence.CaptureTransition(ctx, op.owner.Version(), clock)
	if err != nil {
		return nil, err
	}
	if root.Phase == accepted.Managed {
		op.poll, err = s.opts.Store.CaptureAnchorPoll(ctx, op.owner.Version(), clock)
	} else {
		op.bootstrap, err = s.opts.Store.CaptureAnchorUpdate(ctx, op.owner.Version())
	}
	if err != nil {
		return nil, err
	}
	// This inert boundary holds wmu. Never acquire R, any dependency pin, or
	// call Store inside it. It closes image-read -> driver-capture interposition.
	err = s.opts.Store.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error { return nil })
	if err != nil {
		return nil, err
	}
	if err := op.checkOriginal(); err != nil {
		return nil, err
	}
	return op, nil
}

func anchorImageSettings(image *replication.Image) (*public.Kudurru, error) {
	if image == nil {
		return nil, errAnchorPolicy
	}
	var settings *public.Kudurru
	for _, raw := range image.Payload.Objects {
		var header struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &header) != nil {
			return nil, errAnchorPolicy
		}
		if header.Kind != "Kudurru" {
			continue
		}
		if settings != nil {
			return nil, errAnchorPolicy
		}
		settings = new(public.Kudurru)
		if json.Unmarshal(raw, settings) != nil || settings.APIVersion != public.GroupVersion.String() || settings.Name != "default" || settings.Namespace != "" || settings.UID == "" || settings.ResourceVersion == "" || settings.CreationTimestamp.IsZero() || settings.DeletionTimestamp != nil {
			return nil, errAnchorPolicy
		}
	}
	if settings == nil {
		return nil, errAnchorPolicy
	}
	return settings, nil
}

func anchorPolicyEndpoints(recursion config.Recursion) ([]netip.AddrPort, error) {
	if len(recursion.RootHints) > anchorEndpointLimit {
		return nil, errAnchorPolicy
	}
	endpoints, err := recursor.ParseRootHints(recursion.RootHints)
	if err != nil {
		return nil, errAnchorPolicy
	}
	if len(recursion.RootHints) == 0 {
		endpoints = recursor.DefaultRootHints()
	}
	selected := make([]netip.AddrPort, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if recursion.IPv6 != "off" || endpoint.Addr().Unmap().Is4() {
			selected = append(selected, endpoint)
		}
	}
	if len(selected) == 0 {
		return nil, errAnchorPolicy
	}
	return selected, nil
}

func (op *anchorOperation) checkOriginal() error {
	if op == nil || op.runtime == nil || op.ctx == nil || op.host.Current == nil {
		return errAnchorPolicy
	}
	if err := op.ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := op.ctx.Deadline(); !ok || !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	if !op.host.Current() || op.runtime.life.Err() != nil {
		return serving.ErrDependency
	}
	return op.runtime.writerCurrent(op.ctx, op.target)
}

func (op *anchorOperation) run() (result anchorCompletion, err error) {
	if op == nil || !op.used.CompareAndSwap(false, true) {
		return result, errAnchorPolicy
	}
	if err := op.checkOriginal(); err != nil {
		return result, err
	}
	if op.poll == nil {
		arrived := false
		raw, received, err := acquireAnchorWire(op.wire, func() (trustanchors.Sample, error) {
			arrived = true
			return op.clock.Sample(), nil
		})
		if err != nil {
			if !arrived {
				return result, &anchorAcquisitionFailure{err: err}
			}
			return result, err
		}
		message, err := unpackAnchorBootstrap(raw)
		if err != nil {
			return result, err
		}
		state, observation, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: op.owner.Inspect().ClusterID, PolicyID: op.policy.PolicyID, DS: op.policy.Seeds}, message, received)
		if err != nil {
			return result, err
		}
		err = op.withPin(func(p currentguard.Pinned) error {
			result.Update, err = op.runtime.opts.Store.AnchorCommand(op.bootstrap, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := op.admit(ctx, tx, p); err != nil {
					return err
				}
				_, err := persistence.Bootstrap(ctx, tx, op.original, state, observation)
				return err
			})
			return err
		})
		return result, err
	}
	arrived := false
	raw, stamp, err := acquireAnchorWire(op.wire, func() (sqlite.AnchorReceiptStamp, error) {
		arrived = true
		return op.poll.SampleReceipt()
	})
	if err != nil {
		if !arrived {
			return result, &anchorAcquisitionFailure{err: err}
		}
		return result, err
	}
	receipt, err := op.poll.Receive(raw, stamp)
	if err != nil {
		return result, err
	}
	// Once authenticated, known denial must reach its independent branch even
	// if original root context/host/policy admission now refuses granting work.
	var continuation *sqlite.AnchorPollContinuation
	if receipt.Branch() == sqlite.AnchorPollRevoke {
		result.Denial, continuation, err = op.runtime.opts.Store.CommitAnchorPollDenial(receipt)
		if err != nil {
			return result, err
		}
	}
	err = op.withPin(func(p currentguard.Pinned) error {
		advance := func(ctx context.Context, tx *sqlite.CommandTx, observation trustanchors.Observation) error {
			if err := op.admit(ctx, tx, p); err != nil {
				return err
			}
			_, err := persistence.Advance(ctx, tx, op.original, observation)
			return err
		}
		if continuation != nil {
			result.Update, err = op.runtime.opts.Store.AnchorPollCommandAfterDenial(continuation, advance)
		} else {
			result.Update, err = op.runtime.opts.Store.AnchorPollCommand(receipt, advance)
		}
		return err
	})
	return result, err
}

func (op *anchorOperation) withPin(fn func(currentguard.Pinned) error) error {
	if err := op.checkOriginal(); err != nil {
		return err
	}
	// The retained function already acquires publisher -> resources -> host ->
	// keys -> catalog -> managers. R and wmu are acquired only inside fn.
	return op.runtime.anchorPin(op.ctx, primaryruntime.PublicationFence{Term: op.target.Term, Incarnation: op.target.Incarnation}, fn)
}

func (op *anchorOperation) admit(ctx context.Context, tx *sqlite.CommandTx, pin currentguard.Pinned) error {
	if !tx.Authority().Same(op.target.Term) || tx.Incarnation() != op.target.Incarnation {
		return replication.ErrFenced
	}
	objects, err := tx.List(ctx, "Kudurru", store.ListOptions{Limit: 2})
	if err != nil {
		return err
	}
	if objects.Continue != "" || len(objects.Items) != 1 {
		return errAnchorPolicy
	}
	settings, ok := objects.Items[0].(*public.Kudurru)
	if !ok || store.KeyOf(settings) != store.KeyOf(op.settings) || settings.UID != op.settings.UID || settings.ResourceVersion != op.settings.ResourceVersion || settings.DeletionTimestamp != nil {
		return errAnchorPolicy
	}
	if err := tx.RequireKindUnchanged(ctx, "Kudurru"); err != nil {
		return err
	}
	if pin.CheckTx != nil {
		if err := pin.CheckTx(ctx, tx); err != nil {
			return err
		}
	}
	check := func(context.Context) error {
		if err := op.checkOriginal(); err != nil {
			return err
		}
		if pin.Current == nil || !pin.Current() {
			return serving.ErrDependency
		}
		authority, blocked := op.runtime.opts.Store.ServingAuthority()
		if blocked || !op.token.Matches(authority) {
			return replication.ErrCAS
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return err
	}
	return tx.RequireCommitCheck(check)
}

// Bootstrap has no managed poll object yet. Preflight bounds actual counts and
// offsets before unpacking; trailing data is not silently ignored by Msg.Unpack.
func unpackAnchorBootstrap(raw []byte) (*dns.Msg, error) {
	if len(raw) < 12 || len(raw) > min(anchorWireLimit, trustanchors.MaxWireBytes) {
		return nil, errAnchorWire
	}
	answers, extra := int(binary.BigEndian.Uint16(raw[6:8])), int(binary.BigEndian.Uint16(raw[10:12]))
	if binary.BigEndian.Uint16(raw[4:6]) != 1 || answers == 0 || answers > trustanchors.MaxLiveKeys+trustanchors.MaxSignatures || binary.BigEndian.Uint16(raw[8:10]) != 0 || extra > 1 {
		return nil, errAnchorWire
	}
	_, offset, err := dns.UnpackDomainName(raw, 12)
	if err != nil || offset > len(raw)-4 {
		return nil, errAnchorWire
	}
	offset += 4
	for i := 0; i < answers+extra; i++ {
		_, next, err := dns.UnpackDomainName(raw, offset)
		if err != nil || next > len(raw)-10 {
			return nil, errAnchorWire
		}
		length := int(binary.BigEndian.Uint16(raw[next+8 : next+10]))
		offset = next + 10
		if length > len(raw)-offset {
			return nil, errAnchorWire
		}
		offset += length
	}
	if offset != len(raw) {
		return nil, errAnchorWire
	}
	message := new(dns.Msg)
	if message.Unpack(raw) != nil {
		return nil, errAnchorWire
	}
	return message, nil
}
