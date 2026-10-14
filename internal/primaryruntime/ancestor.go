package primaryruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

type lifecycleLister interface {
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
}

func (r *Runtime) roster(ctx context.Context) ([]*zk.ZoneLifecycle, error) {
	var out []*zk.ZoneLifecycle
	read := func(st lifecycleLister, paginated bool) error {
		next := ""
		seen := map[string]bool{}
		names := map[string]bool{}
		left := 32 << 20
		for page := 0; page < r.o.MaxZones+1; page++ {
			limit := min(16, r.o.MaxZones-len(out))
			if limit <= 0 {
				return ErrCapacity
			}
			items, err := st.List(ctx, zk.KindZoneLifecycle, store.ListOptions{Limit: limit, Continue: next})
			if err != nil {
				return err
			}
			if len(items.Items) > limit {
				return ErrCapacity
			}
			for _, obj := range items.Items {
				l, ok := obj.(*zk.ZoneLifecycle)
				if !ok || l.Validate() != nil || names[l.Name] || l.ResourceVersion == "" {
					return ErrInvalid
				}
				b, err := json.Marshal(l)
				if err != nil {
					return err
				}
				left -= len(b)
				if left < 0 {
					return ErrCapacity
				}
				names[l.Name] = true
				out = append(out, l.DeepCopy())
			}
			if items.Continue == "" {
				return nil
			}
			if !paginated || len(items.Continue) > 4096 || seen[items.Continue] {
				return ErrCapacity
			}
			next = items.Continue
			seen[next] = true
		}
		return ErrCapacity
	}
	if sql, ok := r.o.Store.(*sqlite.Store); ok {
		err := sql.Command(ctx, sql.Authority().Term, func(txctx context.Context, tx *sqlite.CommandTx) error { ctx = txctx; return read(tx, true) })
		return out, err
	}
	err := read(r.o.Store, r.o.Store.Info().Driver == "kube")
	return out, err
}
func (r *Runtime) verifyDescendants(ctx context.Context, parent *zk.ZoneLifecycle) error {
	if parent.Spec.Type != public.ZoneTypePrimary {
		return nil
	}
	all, err := r.roster(ctx)
	if err != nil {
		return err
	}
	var v *zonestate.View
	for _, child := range all {
		if child.Name == parent.Name || child.Spec.Phase == zk.PhaseRetired || child.Spec.Zone.ClusterID != parent.Spec.Zone.ClusterID || child.Spec.Zone.Apex == parent.Spec.Zone.Apex || !dns.IsSubDomain(parent.Spec.Zone.Apex, child.Spec.Zone.Apex) {
			continue
		}
		if v == nil {
			v, err = r.o.Repository.Read(ctx, parent.Spec.HeadZone())
			if err != nil {
				return err
			}
		}
		obligation := ParentObligation(parent, child)
		found := false
		for _, p := range child.Spec.Parents {
			if p.Zone == obligation.Zone && p.Incarnation == obligation.Incarnation && p.BoundaryID == obligation.BoundaryID && !p.Released && (child.Spec.Phase != zk.PhaseActive || p.CommitSequence > 0) {
				found = true
				break
			}
		}
		if !found || !hasFence(v, child.Spec.Zone.Apex) {
			return ErrConflict
		}
	}
	return nil
}

// AdmitAncestor fences a later parent BEFORE extending an existing child's
// obligations. Parent remains unavailable until Activate acknowledges the
// complete roster. Existing obligations can grow, never silently disappear.
func (r *Runtime) AdmitAncestor(ctx context.Context, parentID, childID zk.ZoneID) (*zk.ZoneLifecycle, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	for attempt := 0; attempt < r.o.MaxAttempts; attempt++ {
		parent, err := r.ReadLifecycle(ctx, parentID)
		if err != nil {
			return nil, err
		}
		child, err := r.ReadLifecycle(ctx, childID)
		if err != nil {
			return nil, err
		}
		if parent.Spec.Type != public.ZoneTypePrimary || (parent.Spec.Phase != zk.PhaseFencing && parent.Spec.Phase != zk.PhaseActive) || phaseOrder(child.Spec.Phase) > phaseOrder(zk.PhaseActive) || parentID.Apex == childID.Apex || !dns.IsSubDomain(parentID.Apex, childID.Apex) {
			return nil, ErrInvalid
		}
		v, err := r.o.Repository.Read(ctx, parent.Spec.HeadZone())
		if err != nil {
			return nil, err
		}
		if v.Head.Spec.Lifecycle != zk.LifecycleActive {
			return nil, ErrInvalid
		}
		var prepared *zonestate.Prepared
		if !hasFence(v, childID.Apex) {
			for _, record := range v.Aggregate.Records {
				if record.Provenance.SourceID != "" {
					continue
				}
				rr, _, err := dns.UnpackRR(record.Wire, 0)
				if err != nil {
					return nil, err
				}
				name := canonical(rr.Header().Name)
				if dns.IsSubDomain(childID.Apex, name) || (rr.Header().Rrtype == dns.TypeDNAME && dns.IsSubDomain(name, childID.Apex)) {
					return nil, ErrConflict
				}
			}
			a := *v.Aggregate
			a.Reservations = append(append([]dynamicupdate.Reservation(nil), a.Reservations...), dynamicupdate.Reservation{Name: childID.Apex, Subtree: true, Source: "system"})
			prepared, err = zonestate.Prepare(&a, v.Head, r.o.Limits)
			if err != nil {
				return nil, err
			}
		}
		committed, err := r.admit(ctx, Command{Lifecycle: child, Parent: parent, Expected: v.Expected, Prepared: prepared, Reason: "late-parent-fence"})
		if conflicts(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		obligation := ParentObligation(parent, child)
		obligation.CommitSequence = committed.Head.Spec.CommitSequence
		obligation.RootDigest = committed.Head.Spec.RootDigest
		next := child.DeepCopy()
		next.Spec.Sequence++
		found := false
		for i, p := range next.Spec.Parents {
			if p.Zone == obligation.Zone {
				next.Spec.Parents[i] = obligation
				found = true
				break
			}
		}
		if !found {
			next.Spec.Parents = append(next.Spec.Parents, obligation)
		}
		updated, err := r.Transition(ctx, child, next)
		if conflicts(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		pe, err := r.build(ctx, parent, committed, nil)
		if err != nil {
			return nil, err
		}
		if err := r.install(pe); err != nil {
			return nil, err
		}
		childView, err := r.o.Repository.Read(ctx, child.Spec.HeadZone())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return updated, err
		}
		var config *public.DNSZone
		if childView == nil || childView.Head == nil {
			childView = nil
			config = &public.DNSZone{}
			if err := r.o.Store.Get(ctx, store.Key{Kind: "DNSZone", Name: child.Spec.Zone.ObjectName}, config); err != nil {
				return updated, err
			}
		}
		ce, err := r.build(ctx, updated, childView, config)
		if err != nil {
			return updated, err
		}
		if err := r.install(ce); err != nil {
			return updated, err
		}
		return updated, r.publish(ctx, ce)
	}
	return nil, ErrConflict
}

// Activate is the explicit final parent/child membership barrier. Root's CAS
// MUST recheck the complete actual roster and serialize concurrent creation;
// this method's preflight is not a substitute for transaction admission.
func (r *Runtime) Activate(ctx context.Context, id zk.ZoneID) (*zk.ZoneLifecycle, error) {
	l, err := r.ReadLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	nonprimaryActive := l.Spec.Phase == zk.PhaseActive && l.Spec.Type != public.ZoneTypePrimary
	if l.Spec.Phase != zk.PhaseFencing && !nonprimaryActive {
		return nil, ErrInvalid
	}
	active := l
	if l.Spec.Phase == zk.PhaseFencing {
		next, err := nextPhase(l, zk.PhaseActive)
		if err != nil {
			return nil, err
		}
		active, err = r.Transition(ctx, l, next)
		if err != nil {
			return nil, err
		}
	}
	var v *zonestate.View
	var config *public.DNSZone
	if active.Spec.Type == public.ZoneTypePrimary {
		v, err = r.o.Repository.Read(ctx, active.Spec.HeadZone())
	} else {
		config = new(public.DNSZone)
		err = r.o.Store.Get(ctx, store.Key{Kind: "DNSZone", Name: active.Spec.Zone.ObjectName}, config)
		if err == nil && !imageCurrentSource(active, config) {
			err = ErrConflict
		}
		if err == nil {
			_, err = r.admit(ctx, Command{Lifecycle: active, Reason: "replacement-membership"})
		}
	}
	if err != nil {
		return active, err
	}
	e, err := r.build(ctx, active, v, config)
	if err != nil {
		return active, err
	}
	if err := r.install(e); err != nil {
		return active, err
	}
	return active, r.publish(ctx, e)
}
