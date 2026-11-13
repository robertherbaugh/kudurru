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

// current is serving admission only (D369). It lets this member publish and
// answer from its last accepted managed owner in every role that can hold one,
// including a fenced or conflicted cut, which the Store publishes as DNS-only.
// It is not updater admission: the elected updater separately requires its
// original writable term through writerCurrent, and an unelected role never
// receives a writer token.
func (a *rootAnchorAdmission) current() bool {
	if a == nil || a.owner == nil || a.owner.admission != a || a.life.Err() != nil {
		return false
	}
	if a.edges != nil && (a.edges.owner == nil || a.edges.owner.adopted.Load() != a || a.edges.owner.result.Load() != a.edges) {
		return false
	}
	// DNS output keeps the accepted owner while an observed fence or
	// conflict is persisted (DNSServingAuthority, D369); the updater and
	// every writer still require the write-blocked form.
	authority, blocked := a.owner.store.DNSServingAuthority()
	return !blocked && servingAnchorRole(authority) && authority.MemberID == a.member
}

// servingAnchorRole names every role that may serve accepted managed history.
// An unenrolled receiver has none, and a writer mode without an uncontested
// writable term is refused instead of being guessed at.
func servingAnchorRole(a replication.Authority) bool {
	return a.Writable() || supervisedAnchorRole(a)
}

// supervisedAnchorRole names the roles without an elected updater. They keep
// the independent deny-only supervisor: a follower, and a fenced or conflicted
// member that still serves its last-good DNS. None can advance accepted trust.
func supervisedAnchorRole(a replication.Authority) bool {
	return a.Mode == replication.Follower || a.Mode == replication.Fenced || a.Mode == replication.Conflict
}

// errAnchorNoImage refuses managed topology on a node that holds no installed
// cluster image (D370). The offline join is the only first-image lane: no
// edge is contacted, no root is fetched and nothing is recaptured later, so
// there is nothing for this startup to wait for (D319, D355, D356).
var errAnchorNoImage = errors.New("managed root topology: this node has no installed cluster image; " +
	"finish `kudurru cluster join --offline` first, or run `kudurru cluster join-reconcile --offline` " +
	"after an interrupted join, then start it again (see the runbook \"Add a follower behind managed edges\")")

// anchorWithoutImage names the image-less starts refused with errAnchorNoImage:
// an unenrolled receiver (a join that never installed its first image) for
// either topology, and any node without an installed image for edges. A
// direct follower enrolled without an image keeps waiting for installation,
// which never contacts an edge.
func anchorWithoutImage(a replication.Authority, topology string) bool {
	if a.Mode == replication.Unenrolled {
		return true
	}
	return topology == "edges" && a.Installed == (replication.Generation{})
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
	if !blocked && anchorWithoutImage(authority, topology) {
		return errAnchorNoImage
	}
	if blocked || !servingAnchorRole(authority) || authority.MemberID == "" {
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
// an absent image is never recaptured after a remote observation. A fenced or
// conflicted cut needs the same complete fleet observation and is adopted under
// its own DNS-only disposition (D369); it gains nothing a writable cut lacks.
func (s *servingRuntime) admitAnchorEdges(original context.Context, admission *rootAnchorAdmission) error {
	if s.opts.InternalClient == nil {
		return internalapi.ErrNoCredentials
	}
	_, token, err := s.opts.Store.ReadServingCandidate(original)
	if err != nil {
		return err
	}
	if !servingAnchorRole(token.Authority()) || token.Authority().MemberID != admission.member {
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
	return s.opts.Store.PublishIfCurrent(original, token, sqlite.PublicationOptions{DNSOnly: token.DNSOnly()}, func() error {
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
