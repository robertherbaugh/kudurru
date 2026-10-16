package driver

import (
	"context"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
)

func sameIdentity(l *zk.ZoneLifecycle, z *public.DNSZone) bool {
	return l != nil && z != nil && l.Spec.Zone.ObjectName == z.Name && l.Spec.Zone.UID == string(z.UID) && l.Spec.Zone.Apex == dns.CanonicalName(z.Spec.ZoneName) && l.Spec.Type == z.Spec.Type
}
func retiring(l *zk.ZoneLifecycle) bool {
	return l.Spec.Phase == zk.PhaseRetiring || l.Spec.Phase == zk.PhaseRetired
}
func sourceObservation(l *zk.ZoneLifecycle, z *public.DNSZone) (zk.LifecycleSource, error) {
	if z == nil || string(z.UID) != l.Spec.Zone.UID {
		s := l.Spec.Source
		s.Removed = true
		return s, nil
	}
	token, err := primaryzone.SourceToken(z)
	return zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest}, err
}

func (d *Driver) work(ctx context.Context, w Writer, all roster) error {
	names := make([]string, 0, len(all.zones)+len(all.ledgers))
	for name := range all.zones {
		names = append(names, name)
	}
	for name := range all.ledgers {
		if all.zones[name] == nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	// Bound the combined source/historical roster, not just each half.
	if len(names) > d.o.MaxZones {
		return pr.ErrCapacity
	}
	start := 0
	for i, name := range names {
		if name <= d.after {
			start = i + 1
		}
	}
	now := time.Now()
	count := 0
	for i := 0; i < len(names) && count < d.o.MaxWork; i++ {
		name := names[(start+i)%len(names)]
		d.after = name
		previous := d.retries[name]
		cursor := checkpoint(all.ledgers[name])
		if z := all.zones[name]; z != nil {
			cursor += z.ResourceVersion + ":" + string(z.UID)
		}
		if z := all.zones[name]; z != nil {
			for _, apex := range ancestorNames(dns.CanonicalName(z.Spec.ZoneName)) {
				if parent := all.byApex[apex]; parent != nil && parent.Spec.Type == public.ZoneTypePrimary {
					cursor += parent.ResourceVersion + ":" + string(parent.UID) + ":" + checkpoint(all.ledgers[parent.Name])
				}
			}
		}

		if cursor != previous.cursor {
			previous = retry{}
		}
		if now.Before(previous.at) {
			continue
		}
		count++
		opctx, cancel := context.WithTimeout(ctx, d.o.Timeout)
		err := d.step(opctx, w, all, name)
		cancel()
		status := ZoneStatus{ObjectName: name, Pending: reason(err)}
		if z := all.zones[name]; z != nil {
			status.Apex = dns.CanonicalName(z.Spec.ZoneName)
		}
		if l := all.ledgers[name]; l != nil {
			status.Apex = l.Spec.Zone.Apex
			status.Phase = l.Spec.Phase
			status.Incarnation = l.Spec.Incarnation
			status.Sequence = l.Spec.Sequence
			if m, ok := d.o.Runtime.Capture(now).Member(l.Spec.Zone.Apex); ok {
				status.Version = m.Version
				status.Installed = m.Zone.ObjectName == name
				status.Incarnation, status.Sequence, status.Phase = m.Incarnation, m.Sequence, m.Phase
				status.Published = m.Published
				m.Published = false
				durable, exists := all.prepared.DurableMember(m.Zone)
				status.Acknowledged = exists && reflect.DeepEqual(d.acknowledged[name], m) && reflect.DeepEqual(durable, m)
			}
		}
		next := retry{status: status, cursor: cursor}
		if err != nil {
			next.failures = min(previous.failures+1, 6)
			delay := time.Second * time.Duration(1<<min(next.failures-1, 5))
			// Deterministic bounded per-source jitter avoids synchronized retry bursts.
			jitter := time.Duration(len(name)%17) * delay / 100
			next.at = now.Add(delay + jitter)
			next.status.RetryAt = next.at
		}
		d.retries[name] = next
		if ctx.Err() != nil || !d.current(w) {
			break
		}
	}
	statuses := make([]ZoneStatus, 0, len(names))
	for _, name := range names {
		s := d.retries[name].status
		if s.ObjectName == "" {
			s.ObjectName = name
			s.Pending = "queued"
			if z := all.zones[name]; z != nil {
				s.Apex = dns.CanonicalName(z.Spec.ZoneName)
			}
		}
		statuses = append(statuses, s)
	}
	for name := range d.retries {
		if _, ok := all.zones[name]; !ok && all.ledgers[name] == nil {
			delete(d.retries, name)
			delete(d.acknowledged, name)
		}
	}
	d.mu.Lock()
	d.status.Zones = statuses
	d.mu.Unlock()
	return nil
}

func (d *Driver) parents(all roster, l *zk.ZoneLifecycle) ([]zk.ParentFence, error) {
	parents := []zk.ParentFence{}
	for _, apex := range ancestorNames(l.Spec.Zone.Apex) {
		z := all.byApex[apex]
		if z == nil || z.Spec.Type != public.ZoneTypePrimary {
			continue
		}
		parent := all.ledgers[z.Name]
		if !sameIdentity(parent, z) || retiring(parent) {
			return nil, pr.ErrConflict
		}
		// Do not manufacture obligations to a never-materialized parent. A missing
		// historical head cannot later prove terminal release after source deletion.
		if member, ok := all.prepared.DurableMember(parent.Spec.Zone); !ok || member.Version.CommitSequence == 0 {
			return nil, pr.ErrConflict
		}
		parents = append(parents, pr.ParentObligation(parent, l))
	}
	return parents, nil
}
func ancestorNames(apex string) []string {
	labels := dns.SplitDomainName(apex)
	out := make([]string, 0, len(labels))
	for i := 1; i <= len(labels); i++ {
		out = append(out, strings.Join(labels[i:], ".")+".")
	}
	return out
}
func obsoleteParent(all roster, l *zk.ZoneLifecycle) bool {
	for _, p := range l.Spec.Parents {
		parent := all.ledgers[p.Zone.ObjectName]
		if parent == nil || retiring(parent) || parent.Spec.HeadZone() != p.Zone || parent.Spec.Incarnation != p.Incarnation || !sameIdentity(parent, all.zones[p.Zone.ObjectName]) {
			return true
		}
	}
	return false
}
func (d *Driver) step(ctx context.Context, w Writer, all roster, name string) error {
	if !d.current(w) {
		return pr.ErrUnavailable
	}
	l, z := all.ledgers[name], all.zones[name]
	if l != nil && l.Spec.Sequence == math.MaxUint64 {
		return pr.ErrCapacity
	}
	if l == nil || (l.Spec.Phase == zk.PhaseRetired && z != nil) {
		if l != nil {
			for _, p := range l.Spec.Parents {
				if !p.Released {
					_, err := d.o.Runtime.ReleaseParents(ctx, l.Spec.Zone)
					return err
				}
			}
		}
		if z == nil {
			return nil
		}
		next, err := pr.NewLifecycle(w.Term.ClusterID, z, nil)
		if err != nil {
			return err
		}
		if l != nil {
			if l.Spec.Incarnation == math.MaxUint64 || l.Spec.Sequence == math.MaxUint64 {
				return pr.ErrCapacity
			}
			next.Spec.Incarnation = l.Spec.Incarnation + 1
			next.Spec.Sequence = l.Spec.Sequence + 1
		}
		next.Spec.Parents, err = d.parents(all, next)
		if err != nil {
			return err
		}
		_, err = d.o.Runtime.Transition(ctx, l, next)
		if err == nil {
			d.o.Publication.Wake()
			d.Wake()
		}
		return err
	}
	if l.Spec.Phase == zk.PhaseRetired {
		for _, p := range l.Spec.Parents {
			if !p.Released {
				_, err := d.o.Runtime.ReleaseParents(ctx, l.Spec.Zone)
				return err
			}
		}
		return d.publish(ctx, w, l)
	}
	if l.Spec.Phase == zk.PhaseRetiring || !sameIdentity(l, z) || obsoleteParent(all, l) {
		observed, err := sourceObservation(l, z)
		if err != nil {
			return err
		}
		_, err = d.o.Runtime.RetireSource(ctx, l.Spec.Zone, observed)
		d.o.Publication.Wake()
		return err
	}
	// Source identity refresh and late ancestry are separate durable CAS steps.
	// Primary Reconcile captures its own complete actual zone/RRset/policy read.
	for _, apex := range ancestorNames(l.Spec.Zone.Apex) {
		desired := all.byApex[apex]
		if desired == nil || desired.Spec.Type != public.ZoneTypePrimary {
			continue
		}
		ancestor := all.ledgers[desired.Name]
		if ancestor == nil {
			return pr.ErrConflict
		}
		if ancestor.Spec.Type != public.ZoneTypePrimary || retiring(ancestor) || ancestor.Spec.Zone.Apex == l.Spec.Zone.Apex || !dns.IsSubDomain(ancestor.Spec.Zone.Apex, l.Spec.Zone.Apex) || !sameIdentity(ancestor, all.zones[ancestor.Spec.Zone.ObjectName]) {
			continue
		}
		want := pr.ParentObligation(ancestor, l)
		found := false
		for _, p := range l.Spec.Parents {
			if p.Zone == want.Zone && p.Incarnation == want.Incarnation && p.BoundaryID == want.BoundaryID && p.CommitSequence != 0 && !p.Released {
				found = true
				break
			}
		}
		if !found {
			_, err := d.o.Runtime.AdmitAncestor(ctx, ancestor.Spec.Zone, l.Spec.Zone)
			if err == nil {
				d.Wake()
			}
			return err
		}
	}
	if l.Spec.Phase == zk.PhasePreparing {
		next := l.DeepCopy()
		next.Spec.Sequence++
		next.Spec.Phase = zk.PhaseFencing
		_, err := d.o.Runtime.Transition(ctx, l, next)
		if err == nil {
			d.Wake()
		}
		return err
	}
	if l.Spec.Type == public.ZoneTypePrimary {
		if l.Spec.Phase == zk.PhaseFencing {
			// Prepare the parent before admitting late children; never activate with
			// incomplete descendants. Transaction validation is the final authority.
			if _, err := d.o.Runtime.Prepare(ctx, l.Spec.Zone); err != nil {
				return err
			}
			if _, err := d.o.Runtime.Activate(ctx, l.Spec.Zone); err != nil {
				return err
			}
		} else {
			if _, err := d.o.Runtime.Reconcile(ctx, l.Spec.Zone); err != nil {
				return err
			}
		}
	} else {
		observed, err := sourceObservation(l, z)
		if err != nil {
			return err
		}
		if observed != l.Spec.Source {
			next := l.DeepCopy()
			next.Spec.Sequence++
			next.Spec.Source = observed
			if _, err := d.o.Runtime.Transition(ctx, l, next); err != nil {
				return err
			}
			// The next actual complete read supplies the new source/lifecycle pair.
			d.Wake()
			return nil
		}
		if _, err := d.o.Runtime.Activate(ctx, l.Spec.Zone); err != nil {
			return err
		}
	}
	return d.publish(ctx, w, l)
}
func (d *Driver) publish(ctx context.Context, w Writer, l *zk.ZoneLifecycle) error {
	// Lifecycle may have advanced during this operation. Capture installed memory
	// only for publication, never use it as a source planner or head authority.
	m, ok := d.o.Runtime.Capture(time.Now()).Member(l.Spec.Zone.Apex)
	if !ok || m.Zone != l.Spec.Zone {
		return pr.ErrConflict
	}
	d.o.Publication.Wake()
	if err := d.o.Runtime.Republish(ctx, l.Spec.Zone); err != nil {
		return err
	}
	if err := d.o.Publication.Wait(ctx, w, m); err != nil {
		return err
	}
	m.Published = false
	d.acknowledged[m.Zone.ObjectName] = m
	return nil
}

// Checkpoint formats a stable source/lifecycle cursor for local diagnostics.
func checkpoint(l *zk.ZoneLifecycle) string {
	if l == nil {
		return ""
	}
	return l.ResourceVersion + ":" + strconv.FormatUint(l.Spec.Sequence, 10)
}
