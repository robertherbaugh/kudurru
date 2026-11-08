package main

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// A host declaration alone cannot construct this runtime-owned admission. The
// exact root owner adopts it under finite startup preparation; published inputs
// retain the runtime lifetime, never the canceled preparation context. This
// admission covers direct SQLite deployments or a complete authenticated edge
// fleet. Remote bypass is retained across children by each deployment; it never
// replaces local history, source, writer, publication or output admission.
type rootAnchorAdmission struct {
	owner  *rootAnchorOwner
	life   context.Context
	member string
	edges  *anchorEdgeObservation
}

func (a *rootAnchorAdmission) current() bool {
	if a == nil || a.owner == nil || a.owner.admission != a || a.life.Err() != nil {
		return false
	}
	if a.edges != nil && (a.edges.owner == nil || a.edges.owner.adopted.Load() != a || a.edges.owner.result.Load() != a.edges) {
		return false
	}
	authority, blocked := a.owner.store.ServingAuthority()
	return !blocked && directAnchorRole(authority) && authority.MemberID == a.member
}

func directAnchorRole(a replication.Authority) bool {
	return a.Writable() || a.Mode == replication.Follower
}

func (s *servingRuntime) admitAnchors(ctx context.Context) error {
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.opts.Config.ValidateManagedRootTopology(); err != nil {
		return err
	}
	topology := s.opts.Config.Upstreams.Recursion.ManagedTopology
	if topology != "direct" && topology != "edges" {
		return nil
	}
	authority, blocked := s.opts.Store.ServingAuthority()
	if blocked || !directAnchorRole(authority) || authority.MemberID == "" {
		return errAnchorPolicy
	}
	if err := original.Err(); err != nil {
		return err
	}
	a := &rootAnchorAdmission{owner: s.anchors, life: s.life, member: authority.MemberID}
	if topology == "edges" {
		return s.admitAnchorEdges(original, a)
	}
	s.anchors.admission = a
	if !a.current() {
		return errAnchorPolicy
	}
	return original.Err()
}

// Capture the complete local cut before any remote acquisition. The final
// barrier adopts only the exact issued observation, once, before startup ends.
// Empty followers must install their initial image before using edge topology;
// an absent image is never recaptured after a remote observation.
func (s *servingRuntime) admitAnchorEdges(original context.Context, admission *rootAnchorAdmission) error {
	if s.opts.InternalClient == nil {
		return internalapi.ErrNoCredentials
	}
	_, token, err := s.opts.Store.ReadServingCandidate(original)
	if err != nil {
		return err
	}
	if token.DNSOnly() || token.Authority().MemberID != admission.member {
		return errAnchorPolicy
	}
	fleet := make([]anchorEdgeRequirement, 0, len(s.opts.Config.Upstreams.Recursion.ManagedEdges))
	for _, e := range s.opts.Config.Upstreams.Recursion.ManagedEdges {
		fleet = append(fleet, anchorEdgeRequirement{node: e.Node, address: e.Address, deployment: e.Deployment,
			backend: internalapi.EdgeBypassBackend{Name: s.opts.Config.Node.Name, Address: e.BackendAddress, InternalAddress: e.BackendInternalAddress}})
	}
	attempt, err := captureAnchorEdgeAttempt(original, s.life, s.opts.InternalClient, s.opts.Config.Node.Name, fleet)
	if err != nil {
		return err
	}
	observed, err := attempt.observe()
	if err != nil {
		return err
	}
	return s.opts.Store.PublishIfCurrent(original, token, sqlite.PublicationOptions{}, func() error {
		if err := observed.check(); err != nil {
			return err
		}
		if s.anchors.admission != nil || !attempt.adopted.CompareAndSwap(nil, admission) {
			return errAnchorEdgeEvidence
		}
		// All fallible checks precede these private startup pointer assignments.
		// Subsequent serving retains runtime life, not the work timeout; local
		// source/authority and output gates still apply independently.
		admission.edges = observed
		s.anchors.admission = admission
		return nil
	})
}

// Prepare local dependencies before the FIRST anchor capture/acquisition. This
// bounded startup wait holds the original elected host; it cannot retry an
// anchor operation, change hosts or erase a terminal outcome. No network or
// canonical anchor input has been acquired on this path.
func (s *servingRuntime) prepareAnchorWriter(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture) error {
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		if !s.anchors.admission.current() || host.Current == nil || !host.Current() {
			return errAnchorPolicy
		}
		if err := s.writerCurrent(original, target); err != nil {
			return err
		}
		err := s.anchorPin(original, primaryruntime.PublicationFence{Term: target.Term, Incarnation: target.Incarnation}, func(p currentguard.Pinned) error {
			if p.Current == nil || !p.Current() {
				return serving.ErrDependency
			}
			return nil
		})
		if err == nil {
			return nil
		}
		// Only initial resource readiness waits. Any other actual admission
		// refusal remains terminal, before even starting a raw acquisition.
		if !errors.Is(err, providerresources.ErrClosed) && !errors.Is(err, providerresources.ErrUnavailable) {
			return err
		}
		select {
		case <-original.Done():
			return original.Err()
		case <-timer.C:
		}
	}
}
