package main

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// A host declaration alone cannot construct this runtime-owned admission. The
// exact root owner adopts it under finite startup preparation; published inputs
// retain the runtime lifetime, never the canceled preparation context. This
// admission covers explicit direct SQLite deployments. Enrolled followers have
// a separately joined deny-only supervisor; remote edges remain unavailable.
type rootAnchorAdmission struct {
	owner  *rootAnchorOwner
	life   context.Context
	member string
}

func (a *rootAnchorAdmission) current() bool {
	if a == nil || a.owner == nil || a.owner.admission != a || a.life.Err() != nil {
		return false
	}
	authority, blocked := a.owner.store.ServingAuthority()
	return !blocked && directAnchorRole(authority) && authority.MemberID == a.member
}

func directAnchorRole(a replication.Authority) bool {
	return a.Writable() || a.Mode == replication.Follower
}

func (s *servingRuntime) admitDirectAnchors(ctx context.Context) error {
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.opts.Config.ValidateManagedRootTopology(); err != nil {
		return err
	}
	if s.opts.Config.Upstreams.Recursion.ManagedTopology != "direct" {
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
	s.anchors.admission = a
	if !a.current() {
		return errAnchorPolicy
	}
	return original.Err()
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
