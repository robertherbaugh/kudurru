package sqliteadapter

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func canonical(s string) string            { return dns.CanonicalName(s) }
func descendant(parent, child string) bool { return parent != child && dns.IsSubDomain(parent, child) }
func matchesDesired(l *zk.ZoneLifecycle, z *public.DNSZone) bool {
	return l != nil && z != nil && l.Spec.Zone.ObjectName == z.Name && l.Spec.Zone.UID == string(z.UID) && l.Spec.Zone.Apex == canonical(z.Spec.ZoneName) && l.Spec.Type == z.Spec.Type && l.Spec.Phase != zk.PhaseRetiring && l.Spec.Phase != zk.PhaseRetired
}
func obligation(parent, child *zk.ZoneLifecycle) (zk.ParentFence, bool) {
	want := pr.ParentObligation(parent, child)
	for _, p := range child.Spec.Parents {
		if p.Zone == want.Zone && p.Incarnation == want.Incarnation && p.BoundaryID == want.BoundaryID && !p.Released {
			return p, true
		}
	}
	return zk.ParentFence{}, false
}
func fence(v *zonestate.View, apex string) bool {
	if v == nil || v.Aggregate == nil {
		return false
	}
	for _, res := range v.Aggregate.Reservations {
		if res.Name == apex && res.Type == 0 && res.Subtree && res.Source == "system" {
			return true
		}
	}
	return false
}

// membership checks actual desired objects as well as ledgers. A newly desired
// child without a ledger already denies parent activation/UPDATE; a newly
// desired primary ancestor already denies unfenced child activation/UPDATE.
func (a *Adapter) membership(ctx context.Context, tx *sqlite.CommandTx, all roster, l, except *zk.ZoneLifecycle, active bool) error {
	var parentHead *zonestate.View
	checkChild := func(child *zk.ZoneLifecycle) error {
		p, ok := obligation(l, child)
		if !ok || (child.Spec.Phase == zk.PhaseActive && p.CommitSequence == 0) {
			return pr.ErrConflict
		}
		if parentHead == nil {
			var err error
			parentHead, err = a.readHead(ctx, tx, l.Spec.HeadZone())
			if err != nil {
				return err
			}
		}
		if !fence(parentHead, child.Spec.Zone.Apex) {
			return pr.ErrConflict
		}
		return nil
	}
	for _, z := range all.zones {
		apex := canonical(z.Spec.ZoneName)
		if z.Name == l.Spec.Zone.ObjectName {
			continue
		}
		other := all.ledgers[zk.LifecycleName(l.Spec.Zone.ClusterID, z.Name)]
		if apex == l.Spec.Zone.Apex {
			return pr.ErrConflict
		}
		if z.Spec.Type == public.ZoneTypePrimary && descendant(apex, l.Spec.Zone.Apex) {
			if !matchesDesired(other, z) {
				return pr.ErrConflict
			}
			p, ok := obligation(other, l)
			if !ok {
				return pr.ErrConflict
			}
			if active && p.CommitSequence == 0 {
				return pr.ErrConflict
			}
		}
		if active && l.Spec.Type == public.ZoneTypePrimary && descendant(l.Spec.Zone.Apex, apex) {
			if except != nil && z.Name == except.Spec.Zone.ObjectName {
				continue
			}
			if !matchesDesired(other, z) {
				return pr.ErrConflict
			}
			if err := checkChild(other); err != nil {
				return err
			}
		}
	}
	// Desired deletion is pending intent. A still-admitted descendant remains
	// a boundary until its durable retirement, even after its source is gone.
	if active && l.Spec.Type == public.ZoneTypePrimary {
		for _, child := range all.ledgers {
			if child.Name == l.Name || child.Spec.Phase == zk.PhaseRetired || !descendant(l.Spec.Zone.Apex, child.Spec.Zone.Apex) || (except != nil && child.Name == except.Name) {
				continue
			}
			if err := checkChild(child); err != nil {
				return err
			}
		}
	}
	return a.witnesses(ctx, tx, all, l)
}
func (a *Adapter) witnesses(ctx context.Context, tx *sqlite.CommandTx, all roster, l *zk.ZoneLifecycle) error {
	for _, p := range l.Spec.Parents {
		if p.CommitSequence == 0 {
			if l.Spec.Phase == zk.PhaseActive || p.Released {
				return pr.ErrConflict
			}
			continue
		}
		parent := all.ledgers[zk.LifecycleName(p.Zone.ClusterID, p.Zone.ObjectName)]
		if parent == nil || boundaryID(p, l) != p.BoundaryID {
			return pr.ErrConflict
		}
		v, err := a.readHead(ctx, tx, p.Zone)
		if err != nil {
			return err
		}
		if p.Released && v.Head.Spec.Lifecycle == zk.LifecycleTombstoned {
			terminalParent := (parent.Spec.Incarnation == p.Incarnation && parent.Spec.HeadZone() == p.Zone && parent.Spec.Phase == zk.PhaseRetired) || parent.Spec.Incarnation > p.Incarnation
			if l.Spec.Phase != zk.PhaseRetired || v.Head.Spec.CommitSequence != p.CommitSequence || v.Head.Spec.RootDigest != p.RootDigest || !terminalParent {
				return pr.ErrConflict
			}
			for _, serving := range all.ledgers {
				if serving.Spec.HeadZone() == p.Zone && serving.Spec.Phase != zk.PhaseRetired {
					return pr.ErrConflict
				}
			}
			continue
		}
		if parent.Spec.HeadZone() != p.Zone || parent.Spec.Incarnation != p.Incarnation {
			return pr.ErrConflict
		}
		if v.Head.Spec.CommitSequence < p.CommitSequence || (v.Head.Spec.CommitSequence == p.CommitSequence && v.Head.Spec.RootDigest != p.RootDigest) {
			return pr.ErrConflict
		}
		if p.Released {
			if fence(v, l.Spec.Zone.Apex) {
				return pr.ErrConflict
			}
		} else if l.Spec.Phase != zk.PhaseRetiring && l.Spec.Phase != zk.PhaseRetired {
			if v.Head.Spec.Lifecycle != zk.LifecycleActive || (parent.Spec.Phase != zk.PhaseActive && parent.Spec.Phase != zk.PhaseFencing) || !fence(v, l.Spec.Zone.Apex) {
				return pr.ErrConflict
			}
		}
	}
	return nil
}
func (a *Adapter) candidate(_ context.Context, _ *sqlite.CommandTx, all roster, c pr.Command, current *zonestate.View, next *zonestate.Aggregate) error {
	if c.Prepared.Head.Spec.Zone != c.Expected.Zone {
		return pr.ErrInvalid
	}
	if err := candidateBoundaries(all, c, next); err != nil {
		return err
	}
	if c.Reason == "source-reconcile" {
		return nil
	}
	if current.Head == nil || current.Aggregate == nil {
		return pr.ErrInvalid
	}
	if c.Parent != nil {
		before, after := *current.Aggregate, *next
		before.Reservations = nil
		after.Reservations = nil
		if !reflect.DeepEqual(before, after) {
			return pr.ErrInvalid
		}
		child := c.Lifecycle.Spec.Zone.Apex
		if c.Reason == "parent-release" {
			if fence(&zonestate.View{Aggregate: next}, child) {
				return pr.ErrConflict
			}
			for _, l := range all.ledgers {
				if l.Name == c.Lifecycle.Name {
					continue
				}
				for _, p := range l.Spec.Parents {
					if p.Zone == c.Parent.Spec.HeadZone() && !p.Released && l.Spec.Zone.Apex == child {
						return pr.ErrConflict
					}
				}
			}
		} else if !fence(&zonestate.View{Aggregate: next}, child) {
			return pr.ErrConflict
		}
		// A boundary command can change ONLY the one child's system reservation.
		filter := func(v *zonestate.Aggregate) any {
			out := []dynamicReservation{}
			for _, r := range v.Reservations {
				if r.Name == child && r.Type == 0 && r.Subtree && r.Source == "system" {
					continue
				}
				out = append(out, dynamicReservation{r.Name, r.Type, r.Subtree, r.Source})
			}
			return out
		}
		if !reflect.DeepEqual(filter(current.Aggregate), filter(next)) {
			return pr.ErrInvalid
		}
		return nil
	}
	if c.Reason == "tombstone" || c.Reason == "notify-ack" {
		if !reflect.DeepEqual(current.Aggregate, next) {
			return pr.ErrInvalid
		}
		if c.Reason == "notify-ack" && (c.Prepared.Head.Spec.NotifySequence != 0 || c.Prepared.Head.Spec.NotifySerial != 0) {
			return pr.ErrInvalid
		}
		return nil
	}
	if c.Reason != "update" {
		return pr.ErrInvalid
	}
	if !reflect.DeepEqual(current.Aggregate.Policy, next.Policy) || current.Head.Spec.AuthEpoch != c.Prepared.Head.Spec.AuthEpoch {
		return pr.ErrConflict
	}
	if !reflect.DeepEqual(current.Aggregate.Config, next.Config) || !reflect.DeepEqual(current.Aggregate.Sources, next.Sources) || !reflect.DeepEqual(current.Aggregate.Reservations, next.Reservations) || !current.Aggregate.Expiry.Equal(next.Expiry) {
		return pr.ErrInvalid
	}
	if len(next.Receipts) != len(current.Aggregate.Receipts)+1 {
		return pr.ErrInvalid
	}
	prior := make(map[string]zonestate.Receipt, len(current.Aggregate.Receipts))
	for _, receipt := range current.Aggregate.Receipts {
		prior[receipt.ID] = receipt
	}
	for _, receipt := range next.Receipts {
		if old, ok := prior[receipt.ID]; ok {
			if old != receipt {
				return pr.ErrInvalid
			}
			delete(prior, receipt.ID)
		} else if receipt.Principal != c.Proof.Principal || receipt.AuthEpoch != current.Head.Spec.AuthEpoch || receipt.CommitSequence != c.Prepared.Head.Spec.CommitSequence || receipt.Serial != c.Prepared.Head.Spec.Serial || receipt.Rcode != dns.RcodeSuccess {
			return pr.ErrInvalid
		}
	}
	if len(prior) != 0 {
		return pr.ErrInvalid
	}
	rrs, err := next.RRs()
	if err != nil {
		return err
	}
	// Reuse the foundation's independent static ownership/SOA-only-serial
	// checks even though the trusted engine has already validated its plan.
	_, err = zonestate.ApplyResult(current.Aggregate, &dynamicupdate.Result{Records: rrs, Serial: c.Prepared.Head.Spec.Serial, AuthEpoch: current.Head.Spec.AuthEpoch, CredentialVersion: c.Proof.Principal.Version}, c.Proof.Principal)
	if err != nil {
		return err
	}
	return nil
}

func candidateBoundaries(all roster, c pr.Command, next *zonestate.Aggregate) error {
	target := c.Lifecycle
	if c.Parent != nil {
		target = c.Parent
	}
	boundaries := map[string]bool{}
	if target.Spec.Phase == zk.PhaseActive {
		for _, z := range all.zones {
			apex := canonical(z.Spec.ZoneName)
			if descendant(target.Spec.Zone.Apex, apex) {
				if c.Reason == "parent-release" && z.Name == c.Lifecycle.Spec.Zone.ObjectName {
					continue
				}
				boundaries[apex] = true
			}
		}
		for _, child := range all.ledgers {
			if child.Name == target.Name || !descendant(target.Spec.Zone.Apex, child.Spec.Zone.Apex) || (c.Reason == "parent-release" && child.Name == c.Lifecycle.Name) {
				continue
			}
			if _, needed := obligation(target, child); needed {
				boundaries[child.Spec.Zone.Apex] = true
			}
		}
	}
	if c.Parent != nil && c.Reason != "parent-release" {
		boundaries[c.Lifecycle.Spec.Zone.Apex] = true
	}
	for apex := range boundaries {
		if !fence(&zonestate.View{Aggregate: next}, apex) {
			return pr.ErrConflict
		}
	}
	for _, record := range next.Records {
		if record.Provenance.SourceID != "" {
			continue
		}
		rr, n, err := dns.UnpackRR(record.Wire, 0)
		if err != nil || n != len(record.Wire) {
			return pr.ErrInvalid
		}
		name := canonical(rr.Header().Name)
		for apex := range boundaries {
			if dns.IsSubDomain(apex, name) || (rr.Header().Rrtype == dns.TypeDNAME && dns.IsSubDomain(name, apex)) {
				return pr.ErrConflict
			}
		}
	}
	return nil
}

type dynamicReservation struct {
	Name    string
	Type    uint16
	Subtree bool
	Source  string
}

func boundaryID(p zk.ParentFence, child *zk.ZoneLifecycle) string {
	b, _ := json.Marshal(struct {
		Parent, Child                       zk.ZoneID
		ParentIncarnation, ChildIncarnation uint64
	}{p.Zone, child.Spec.Zone, p.Incarnation, child.Spec.Incarnation})
	return zk.Digest(b)
}
