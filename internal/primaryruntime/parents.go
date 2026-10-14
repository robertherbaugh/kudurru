package primaryruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miekg/dns"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// ParentObligation binds a root-captured ancestor incarnation to this child.
func ParentObligation(parent, child *zk.ZoneLifecycle) zk.ParentFence {
	b, _ := json.Marshal(struct {
		Parent, Child                       zk.ZoneID
		ParentIncarnation, ChildIncarnation uint64
	}{parent.Spec.HeadZone(), child.Spec.Zone, parent.Spec.Incarnation, child.Spec.Incarnation})
	return zk.ParentFence{Zone: parent.Spec.HeadZone(), Incarnation: parent.Spec.Incarnation, BoundaryID: zk.Digest(b)}
}
func hasFence(v *zonestate.View, apex string) bool {
	if v == nil || v.Aggregate == nil {
		return false
	}
	for _, p := range v.Aggregate.Reservations {
		if canonical(p.Name) == apex && p.Type == 0 && p.Subtree && p.Source == "system" {
			return true
		}
	}
	return false
}
func (r *Runtime) readParent(ctx context.Context, p zk.ParentFence) (*zk.ZoneLifecycle, *zonestate.View, error) {
	return r.readParentPhase(ctx, p, false)
}
func (r *Runtime) readParentPhase(ctx context.Context, p zk.ParentFence, allowFencing bool) (*zk.ZoneLifecycle, *zonestate.View, error) {
	var l zk.ZoneLifecycle
	if err := r.o.Store.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(p.Zone.ClusterID, p.Zone.ObjectName)}, &l); err != nil {
		return nil, nil, err
	}
	if l.Validate() != nil || l.ResourceVersion == "" || l.Spec.HeadZone() != p.Zone || l.Spec.Incarnation != p.Incarnation || (l.Spec.Phase != zk.PhaseActive && (!allowFencing || l.Spec.Phase != zk.PhaseFencing)) {
		return nil, nil, ErrConflict
	}
	v, err := r.o.Repository.Read(ctx, p.Zone)
	if err != nil {
		return nil, nil, err
	}
	if v.Head.Spec.Lifecycle != zk.LifecycleActive {
		return nil, nil, ErrConflict
	}
	return l.DeepCopy(), v, nil
}
func (r *Runtime) verifyParents(ctx context.Context, l *zk.ZoneLifecycle) error {
	for _, p := range l.Spec.Parents {
		parent, v, err := r.readParentPhase(ctx, p, true)
		if err != nil {
			return err
		}
		if ParentObligation(parent, l).BoundaryID != p.BoundaryID || p.Released || p.CommitSequence == 0 || v.Head.Spec.CommitSequence < p.CommitSequence || (v.Head.Spec.CommitSequence == p.CommitSequence && v.Head.Spec.RootDigest != p.RootDigest) || !hasFence(v, l.Spec.Zone.Apex) {
			return ErrConflict
		}
	}
	return nil
}

// FenceParents installs every ancestor reservation before child activation.
// Each head and the obligation ledger are separate CAS operations. Failures
// retain conservative coverage/fences; no multi-head transaction is claimed.
func (r *Runtime) FenceParents(ctx context.Context, id zk.ZoneID) (*zk.ZoneLifecycle, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	for attempt := 0; attempt < r.o.MaxAttempts; attempt++ {
		l, err := r.ReadLifecycle(ctx, id)
		if err != nil {
			return nil, err
		}
		if l.Spec.Phase == zk.PhasePreparing {
			next, _ := nextPhase(l, zk.PhaseFencing)
			l, err = r.Transition(ctx, l, next)
			if conflicts(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		if l.Spec.Phase != zk.PhaseFencing {
			return nil, ErrInvalid
		}
		next := l.DeepCopy()
		retry := false
		for i, p := range l.Spec.Parents {
			parent, v, err := r.readParent(ctx, p)
			if err != nil {
				return nil, err
			}
			if ParentObligation(parent, l).BoundaryID != p.BoundaryID {
				return nil, ErrInvalid
			}
			var prepared *zonestate.Prepared
			if !hasFence(v, id.Apex) {
				for _, record := range v.Aggregate.Records {
					rr, _, err := dns.UnpackRR(record.Wire, 0)
					if err != nil {
						return nil, err
					}
					name := canonical(rr.Header().Name)
					if record.Provenance.SourceID == "" && (dns.IsSubDomain(id.Apex, name) || (rr.Header().Rrtype == dns.TypeDNAME && dns.IsSubDomain(name, id.Apex))) {
						return nil, fmt.Errorf("%w: existing dynamic child-boundary data", ErrConflict)
					}
				}
				a := *v.Aggregate
				a.Reservations = append(append([]dynamicupdate.Reservation(nil), a.Reservations...), dynamicupdate.Reservation{Name: id.Apex, Subtree: true, Source: "system"})
				prepared, err = zonestate.Prepare(&a, v.Head, r.o.Limits)
				if err != nil {
					return nil, err
				}
			}
			committed, err := r.admit(ctx, Command{Lifecycle: l, Parent: parent, Expected: v.Expected, Prepared: prepared, Reason: "parent-fence"})
			if conflicts(err) {
				retry = true
				break
			}
			if err != nil {
				return nil, err
			}
			e, err := r.build(ctx, parent, committed, nil)
			if err != nil {
				return nil, err
			}
			if err := r.install(e); err != nil {
				return nil, err
			}
			if err := r.publish(ctx, e); err != nil {
				return nil, err
			}
			next.Spec.Parents[i].CommitSequence = committed.Head.Spec.CommitSequence
			next.Spec.Parents[i].RootDigest = committed.Head.Spec.RootDigest
		}
		if retry {
			continue
		}
		next.Spec.Sequence++
		out, err := r.Transition(ctx, l, next)
		if conflicts(err) {
			continue
		}
		return out, err
	}
	return nil, ErrConflict
}

// ReleaseParents is legal only after durable child retirement. The child
// coverage remains until root publishes an explicit replacement/removal cut.
func (r *Runtime) ReleaseParents(ctx context.Context, id zk.ZoneID) (*zk.ZoneLifecycle, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	for attempt := 0; attempt < r.o.MaxAttempts; attempt++ {
		l, err := r.ReadLifecycle(ctx, id)
		if err != nil {
			return nil, err
		}
		if l.Spec.Phase != zk.PhaseRetired {
			return nil, ErrInvalid
		}
		next := l.DeepCopy()
		retry := false
		for i, p := range l.Spec.Parents {
			if p.Released {
				continue
			}
			parent, v, terminal, err := r.releaseParent(ctx, p)
			if err != nil {
				return nil, err
			}
			if imageBoundaryID(p, l) != p.BoundaryID {
				return nil, ErrInvalid
			}
			if terminal {
				// Admission rechecks the exact terminal head and complete roster in the
				// child lifecycle CAS. Historical reservations/data remain untouched.
				next.Spec.Parents[i].Released = true
				next.Spec.Parents[i].CommitSequence = v.Head.Spec.CommitSequence
				next.Spec.Parents[i].RootDigest = v.Head.Spec.RootDigest
				continue
			}
			// Root admission also checks that no other lifecycle obligation still
			// needs this identical boundary before allowing its release.
			a := *v.Aggregate
			a.Reservations = nil
			for _, res := range v.Aggregate.Reservations {
				if canonical(res.Name) != id.Apex || res.Type != 0 || !res.Subtree || res.Source != "system" {
					a.Reservations = append(a.Reservations, res)
				}
			}
			var prepared *zonestate.Prepared
			if hasFence(v, id.Apex) {
				prepared, err = zonestate.Prepare(&a, v.Head, r.o.Limits)
				if err != nil {
					return nil, err
				}
			}
			committed, err := r.admit(ctx, Command{Lifecycle: l, Parent: parent, Expected: v.Expected, Prepared: prepared, Reason: "parent-release"})
			if conflicts(err) {
				retry = true
				break
			}
			if err != nil {
				return nil, err
			}
			e, err := r.build(ctx, parent, committed, nil)
			if err != nil {
				return nil, err
			}
			if err := r.install(e); err != nil {
				return nil, err
			}
			// An active parent may be unavailable while a desired surviving child
			// is retired/replacing. Releasing its retired boundary cannot wait for
			// active routing acknowledgment: that would prevent the replacement.
			// Install unpublished; the final exact retired-child witness is published.
			next.Spec.Parents[i].Released = true
			next.Spec.Parents[i].CommitSequence = committed.Head.Spec.CommitSequence
			next.Spec.Parents[i].RootDigest = committed.Head.Spec.RootDigest
		}
		if retry {
			continue
		}
		next.Spec.Sequence++
		out, err := r.Transition(ctx, l, next)
		if conflicts(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		v, readErr := r.o.Repository.Read(ctx, out.Spec.HeadZone())
		if readErr != nil && !errors.Is(readErr, store.ErrNotFound) {
			return out, readErr
		}
		var e *entry
		if v != nil && v.Head != nil {
			e, err = r.build(ctx, out, v, nil)
		} else {
			e, err = r.retirementCoverage(ctx, out)
		}
		if err != nil {
			return out, err
		}
		if err := r.install(e); err != nil {
			return out, err
		}
		return out, r.publish(ctx, e)
	}
	return nil, ErrConflict
}

// releaseParent distinguishes an active boundary from explicit terminal proof.
// Missing heads are never proof; the adapter validates the roster in the CAS.
func (r *Runtime) releaseParent(ctx context.Context, p zk.ParentFence) (*zk.ZoneLifecycle, *zonestate.View, bool, error) {
	var parent zk.ZoneLifecycle
	if err := r.o.Store.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(p.Zone.ClusterID, p.Zone.ObjectName)}, &parent); err != nil {
		return nil, nil, false, err
	}
	if parent.Validate() != nil || parent.ResourceVersion == "" {
		return nil, nil, false, ErrInvalid
	}
	v, err := r.o.Repository.Read(ctx, p.Zone)
	if err != nil {
		return nil, nil, false, err
	}
	if v == nil || v.Head == nil {
		return nil, nil, false, ErrInvalid
	}
	if v.Head.Spec.Lifecycle == zk.LifecycleTombstoned && ((parent.Spec.Incarnation == p.Incarnation && parent.Spec.HeadZone() == p.Zone && parent.Spec.Phase == zk.PhaseRetired) || parent.Spec.Incarnation > p.Incarnation) {
		return parent.DeepCopy(), v, true, nil
	}
	if parent.Spec.HeadZone() != p.Zone || parent.Spec.Incarnation != p.Incarnation || parent.Spec.Phase != zk.PhaseActive || v.Head.Spec.Lifecycle != zk.LifecycleActive {
		return nil, nil, false, ErrConflict
	}
	return parent.DeepCopy(), v, false, nil
}
