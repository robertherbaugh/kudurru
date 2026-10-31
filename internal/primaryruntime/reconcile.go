package primaryruntime

import (
	"context"
	"encoding/json"
	"errors"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/types"
)

// Reconcile refreshes actual inputs and lifecycle source identity on every
// retry. Missing/incomplete sources never prune or deactivate membership.
func (r *Runtime) Reconcile(ctx context.Context, id zk.ZoneID) (*zonestate.View, error) {
	return r.reconcile(ctx, id, true)
}

// Prepare builds/fences a pending head while retaining unavailable membership.
// Root uses it before AdmitAncestor/Activate when children already exist.
func (r *Runtime) Prepare(ctx context.Context, id zk.ZoneID) (*zonestate.View, error) {
	return r.reconcile(ctx, id, false)
}
func (r *Runtime) reconcile(ctx context.Context, id zk.ZoneID, activate bool) (*zonestate.View, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	if r.o.Reader == nil || r.o.Admit == nil {
		return nil, ErrUnavailable
	}
	for attempt := 0; attempt < r.o.MaxAttempts; attempt++ {
		l, err := r.ReadLifecycle(ctx, id)
		if err != nil {
			return nil, err
		}
		if l.Spec.Type != public.ZoneTypePrimary || (l.Spec.Phase != zk.PhasePreparing && l.Spec.Phase != zk.PhaseFencing && l.Spec.Phase != zk.PhaseActive) {
			return nil, ErrInvalid
		}
		needsFences := l.Spec.Phase == zk.PhasePreparing
		if l.Spec.Phase == zk.PhaseFencing {
			for _, parent := range l.Spec.Parents {
				if parent.CommitSequence == 0 {
					needsFences = true
					break
				}
			}
		}
		if needsFences {
			l, err = r.FenceParents(ctx, id)
			// FenceParents owns bounded fresh-CAS retries. A surfaced conflict
			// can also mean existing dynamic boundary data; return it before
			// compiling a child head or attempting witness-free activation.
			if err != nil {
				return nil, err
			}
		}
		base, err := r.o.Repository.Read(ctx, l.Spec.HeadZone())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if base == nil || (base.Head != nil && base.Head.Spec.Lifecycle != zk.LifecycleActive) {
			return nil, ErrInvalid
		}
		in, err := r.o.Reader.ReadDesired(ctx, l.Spec.Zone)
		if err != nil {
			return nil, err
		}
		if in == nil || !in.Complete || in.Zone == nil {
			return nil, ErrInvalid
		}
		token, err := primaryzone.SourceToken(in.Zone)
		if err != nil {
			return nil, err
		}
		source := zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest}
		if source != l.Spec.Source {
			next := l.DeepCopy()
			next.Spec.Sequence++
			next.Spec.Source = source
			l, err = r.Transition(ctx, l, next)
			if conflicts(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		config := in.Zone.DeepCopy()
		if base.Head == nil {
			e, err := r.build(ctx, l, nil, config)
			if err != nil {
				return nil, err
			}
			if err := r.install(e); err != nil {
				return nil, err
			}
		}
		mapped := *in
		mapped.Zone = config.DeepCopy()
		mapped.Zone.UID = types.UID(l.Spec.HeadZone().UID)
		mapped.Targets = append([]zonestate.DesiredTarget(nil), in.Targets...)
		for i := range mapped.Targets {
			mapped.Targets[i].Zone = l.Spec.HeadZone()
		}
		compiled, err := r.o.Compiler.Project(ctx, base, &mapped, r.o.Now())
		if err != nil {
			return nil, err
		}
		if compiled == nil || compiled.Aggregate == nil {
			return nil, ErrInvalid
		}
		if err := restoreActualZoneReceipt(base, compiled.Aggregate, config, in.Targets); err != nil {
			return nil, err
		}
		p, err := zonestate.Prepare(compiled.Aggregate, base.Head, r.o.Limits)
		if err != nil {
			return nil, err
		}
		candidate := &zonestate.View{Head: p.Head, Aggregate: compiled.Aggregate}
		// Opaque prebuild validates the real immutable primary BEFORE admission.
		m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: primaryzone.BuildCommitted, Limits: r.o.Limits})
		built, err := m.Prepare(ctx, candidate)
		if err != nil {
			return nil, err
		}
		command := Command{Lifecycle: l, Expected: base.Expected, Prepared: p, Reason: "source-reconcile", Desired: in}
		if base.Head != nil && p.Head.Spec.RootDigest == base.Head.Spec.RootDigest {
			command.Prepared = nil
		}
		v, err := r.admit(ctx, command)
		if conflicts(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if command.Prepared != nil {
			if err := m.Install(ctx, v, built); err != nil {
				return v, err
			}
		}
		if activate && l.Spec.Phase == zk.PhaseFencing {
			next, err := nextPhase(l, zk.PhaseActive)
			if err != nil {
				return v, err
			}
			l, err = r.Transition(ctx, l, next)
			if conflicts(err) {
				continue
			}
			if err != nil {
				return v, err
			}
		}
		e, err := r.build(ctx, l, v, nil)
		if err != nil {
			return v, err
		}
		if err := r.install(e); err != nil {
			return v, err
		}
		return v, r.publish(ctx, e)
	}
	return nil, ErrConflict
}

// Only frozen serving config uses the incarnation UID; desired receipts must
// truthfully retain the actual source UID, relevant intent and observed RV.
func restoreActualZoneReceipt(base *zonestate.View, a *zonestate.Aggregate, z *public.DNSZone, targets []zonestate.DesiredTarget) error {
	token, err := primaryzone.SourceToken(z)
	if err != nil {
		return err
	}
	for i, s := range a.Sources {
		if s.ID != token.ID {
			continue
		}
		e, err := zonestate.DecodeSourceEnvelope(s.Desired)
		if err != nil {
			return err
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(e.Object, &object); err != nil {
			return err
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(object["metadata"], &metadata); err != nil {
			return err
		}
		metadata["uid"], err = json.Marshal(token.UID)
		if err != nil {
			return err
		}
		object["metadata"], err = json.Marshal(metadata)
		if err != nil {
			return err
		}
		e.Object, err = json.Marshal(object)
		if err != nil {
			return err
		}
		e.IntentDigest = token.IntentDigest
		s.UID = token.UID
		s.ResourceVersion = token.ResourceVersion
		e.Acknowledged = nil
		if base.Aggregate != nil {
			for _, old := range base.Aggregate.Sources {
				if old.ID == s.ID && old.UID == s.UID {
					prior, err := zonestate.DecodeSourceEnvelope(old.Desired)
					if err == nil {
						e.Acknowledged = prior.Acknowledged
						if prior.IntentDigest == e.IntentDigest && prior.Projection == e.Projection && prior.Reason == e.Reason {
							s.ResourceVersion = old.ResourceVersion
						}
					}
					break
				}
			}
		}
		if err := zonestate.AcknowledgeSource(&e, s, targets); err != nil {
			return err
		}
		s.Desired, err = zonestate.EncodeSourceEnvelope(e)
		if err != nil {
			return err
		}
		a.Sources[i] = s
		return nil
	}
	return ErrInvalid
}
