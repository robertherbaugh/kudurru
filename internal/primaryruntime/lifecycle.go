package primaryruntime

import (
	"context"
	"errors"
	"math"
	"reflect"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func phaseOrder(p string) int {
	switch p {
	case zk.PhasePreparing:
		return 1
	case zk.PhaseFencing:
		return 2
	case zk.PhaseActive:
		return 3
	case zk.PhaseRetiring:
		return 4
	case zk.PhaseRetired:
		return 5
	}
	return 0
}

// NewLifecycle constructs detached intent; only Transition's driver callback
// can admit it. Parents must contain the complete root-captured ancestor cut.
func NewLifecycle(cluster string, z *public.DNSZone, parents []zk.ParentFence) (*zk.ZoneLifecycle, error) {
	if z == nil {
		return nil, ErrInvalid
	}
	token, err := primaryzone.SourceToken(z)
	if err != nil {
		return nil, err
	}
	l := &zk.ZoneLifecycle{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: zk.KindZoneLifecycle}, Spec: zk.ZoneLifecycleSpec{FormatVersion: zk.FormatVersion, Zone: zk.ZoneID{ClusterID: cluster, ObjectName: z.Name, UID: string(z.UID), Apex: canonical(z.Spec.ZoneName)}, Type: z.Spec.Type, Incarnation: 1, Sequence: 1, Phase: zk.PhasePreparing, Source: zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest}, Parents: append([]zk.ParentFence(nil), parents...)}}
	l.Name = zk.LifecycleName(cluster, z.Name)
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

// ReadLifecycle reads an exact actual source identity from the private ledger.
func (r *Runtime) ReadLifecycle(ctx context.Context, id zk.ZoneID) (*zk.ZoneLifecycle, error) {
	var l zk.ZoneLifecycle
	if err := r.o.Store.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(id.ClusterID, id.ObjectName)}, &l); err != nil {
		return nil, err
	}
	if l.Validate() != nil || l.ResourceVersion == "" || l.Spec.Zone != id {
		return nil, ErrConflict
	}
	return l.DeepCopy(), nil
}

// Transition admits exactly current -> next; a conflict is returned for fresh
// caller reconstruction. No later object RV is applied to an old transition.
func (r *Runtime) Transition(ctx context.Context, current, next *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error) {
	if r.o.LifecycleCAS == nil {
		return nil, ErrUnavailable
	}
	rv := ""
	if current != nil {
		rv = current.ResourceVersion
	}
	if zk.ValidateLifecycleTransition(current, rv, next) != nil {
		return nil, ErrInvalid
	}
	if next.Spec.Phase == zk.PhaseActive {
		if err := r.verifyDescendants(ctx, next); err != nil {
			return nil, err
		}
		if err := r.verifyParents(ctx, next); err != nil {
			return nil, err
		}
		if next.Spec.Type == public.ZoneTypePrimary {
			v, err := r.o.Repository.Read(ctx, next.Spec.HeadZone())
			if err != nil {
				return nil, err
			}
			if v.Head.Spec.Lifecycle != zk.LifecycleActive {
				return nil, ErrInvalid
			}
		}
	}
	out, err := r.o.LifecycleCAS(ctx, copyLifecycle(current), next.DeepCopy())
	if err != nil {
		return nil, err
	}
	if out == nil || out.Validate() != nil || out.ResourceVersion == "" || !reflect.DeepEqual(out.Spec, next.Spec) {
		return nil, ErrInvalid
	}
	return out.DeepCopy(), nil
}
func copyLifecycle(l *zk.ZoneLifecycle) *zk.ZoneLifecycle { return l.DeepCopy() }
func nextPhase(l *zk.ZoneLifecycle, phase string) (*zk.ZoneLifecycle, error) {
	if l == nil || l.Spec.Sequence == math.MaxUint64 {
		return nil, ErrInvalid
	}
	out := l.DeepCopy()
	out.Spec.Sequence++
	out.Spec.Phase = phase
	return out, nil
}

func (r *Runtime) admit(ctx context.Context, c Command) (*zonestate.View, error) {
	if r.o.Admit == nil {
		return nil, ErrUnavailable
	}
	c.Lifecycle = copyLifecycle(c.Lifecycle)
	c.Parent = copyLifecycle(c.Parent)
	v, err := r.o.Admit(ctx, c)
	if err != nil {
		return nil, err
	}
	if c.Prepared != nil {
		if v == nil || v.Head == nil || v.Head.ResourceVersion == "" || v.Head.Spec != c.Prepared.Head.Spec {
			return nil, ErrInvalid
		}
	} else if c.Expected.ResourceVersion != "" {
		if v == nil || v.Head == nil || v.Head.ResourceVersion != c.Expected.ResourceVersion || version(v) != (zonestate.Version{Zone: c.Expected.Zone, CommitSequence: c.Expected.CommitSequence, RootDigest: c.Expected.RootDigest}) {
			return nil, ErrConflict
		}
	}
	return v, nil
}
func conflicts(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, store.ErrConflict) || errors.Is(err, zonestate.ErrConflict)
}

// Retire closes membership admission before tombstoning the exact current head,
// then durably retires the incarnation. Parent obligations remain until release.
func (r *Runtime) Retire(ctx context.Context, id zk.ZoneID) (*zk.ZoneLifecycle, error) {
	return r.retire(ctx, id, nil)
}

// RetireSource binds an explicit root-observed delete or replacement token.
// The driver checks this CURRENT observation, not the original accepted RV.
// Removed means verified deletion/former-UID replacement, never read failure.
func (r *Runtime) RetireSource(ctx context.Context, id zk.ZoneID, source zk.LifecycleSource) (*zk.ZoneLifecycle, error) {
	return r.retire(ctx, id, &source)
}
func (r *Runtime) retire(ctx context.Context, id zk.ZoneID, source *zk.LifecycleSource) (*zk.ZoneLifecycle, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	for attempt := 0; attempt < r.o.MaxAttempts; attempt++ {
		l, err := r.ReadLifecycle(ctx, id)
		if err != nil {
			return nil, err
		}
		if l.Spec.Phase == zk.PhaseRetired {
			return l, nil
		}
		if l.Spec.Phase != zk.PhaseRetiring || (source != nil && l.Spec.Source != *source) {
			next, err := nextPhase(l, zk.PhaseRetiring)
			if err != nil {
				return nil, err
			}
			if source != nil {
				next.Spec.Source = *source
			}
			l, err = r.Transition(ctx, l, next)
			if conflicts(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		v, err := r.o.Repository.Read(ctx, l.Spec.HeadZone())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if v == nil {
			return nil, ErrInvalid
		}
		if v.Head == nil {
			e, err := r.retirementCoverage(ctx, l)
			if err != nil {
				return nil, err
			}
			if err := r.install(e); err != nil {
				return nil, err
			}
		}
		if v.Head != nil {
			e, err := r.build(ctx, l, v, nil)
			if err != nil {
				return nil, err
			}
			if err := r.install(e); err != nil {
				return nil, err
			}
			if v.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
				p, err := zonestate.Prepare(v.Aggregate, v.Head, r.o.Limits)
				if err != nil {
					return nil, err
				}
				p.Head.Spec.Lifecycle = zk.LifecycleTombstoned
				v, err = r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Prepared: p, Reason: "tombstone"})
				if conflicts(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
			}
		}
		next, err := nextPhase(l, zk.PhaseRetired)
		if err != nil {
			return nil, err
		}
		retired, err := r.Transition(ctx, l, next)
		if conflicts(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		{
			var e *entry
			var err error
			if v.Head != nil {
				e, err = r.build(ctx, retired, v, nil)
			} else {
				e, err = r.retirementCoverage(ctx, retired)
			}
			if err != nil {
				return nil, err
			}
			if err := r.install(e); err != nil {
				return nil, err
			}
			if err := r.publish(ctx, e); err != nil {
				return retired, err
			}
		}
		return retired, nil
	}
	return nil, ErrConflict
}

// Nonprimary incarnations have no RR head; their membership still must close
// before retirement/replacement. Preserve captured ACL coverage when available.
func (r *Runtime) retirementCoverage(ctx context.Context, l *zk.ZoneLifecycle) (*entry, error) {
	r.mu.RLock()
	old := r.entries[l.Spec.Zone.Apex]
	r.mu.RUnlock()
	if old != nil && old.lifecycle.Name == l.Name && old.member.Incarnation == l.Spec.Incarnation {
		cp := *old
		cp.lifecycle = l.DeepCopy()
		cp.member.Phase = l.Spec.Phase
		cp.member.Sequence = l.Spec.Sequence
		cp.member.Parents = append([]zk.ParentFence(nil), l.Spec.Parents...)
		cp.member.Published = false
		cp.model = cp.coverage
		return &cp, nil
	}
	config := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: l.Spec.Zone.ObjectName, UID: types.UID(l.Spec.Zone.UID)}, Spec: public.DNSZoneSpec{ZoneName: l.Spec.Zone.Apex, Type: public.ZoneTypePrimary}}
	return r.build(ctx, l, nil, config)
}

// InstallReplacement publishes an explicitly admitted nonprimary incarnation.
// Same source UID is safe because incarnation and retired membership fence it;
// Materializer.ReplaceRetired is deliberately not used for this type cut.
func (r *Runtime) InstallReplacement(ctx context.Context, l *zk.ZoneLifecycle, model *zone.Zone) error {
	if l == nil || l.Validate() != nil || l.Spec.Phase != zk.PhaseActive || l.Spec.Type == public.ZoneTypePrimary || model == nil || model.Name() != l.Spec.Zone.Apex || model.Type() != l.Spec.Type {
		return ErrInvalid
	}
	if _, err := r.admit(ctx, Command{Lifecycle: l, Reason: "replacement-membership"}); err != nil {
		return err
	}
	cp := l.DeepCopy()
	coverageConfig := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: l.Spec.Zone.ObjectName, UID: types.UID(l.Spec.Zone.UID)}, Spec: public.DNSZoneSpec{ZoneName: l.Spec.Zone.Apex, Type: public.ZoneTypePrimary}}
	e, err := r.build(ctx, cp, nil, coverageConfig)
	if err != nil {
		return err
	}
	e.model = model
	if err := r.install(e); err != nil {
		return err
	}
	return r.publish(ctx, e)
}
