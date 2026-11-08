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
// tranche admits only explicit direct standalone deployments, not remote edges
// or enrolled followers whose local denial supervisor is not yet mounted.
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
	return !blocked && authority.Mode == replication.Standalone && authority.MemberID == a.member && authority.Writable()
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
	if blocked || authority.Mode != replication.Standalone || !authority.Writable() || authority.MemberID == "" {
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
