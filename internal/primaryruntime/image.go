package primaryruntime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// ImageOptions names the complete LOCAL registry, never one inferred from the
// candidate. Validation includes zero-count kinds and optional owner validators.
// Scheme and validators must remain immutable during preparation. Now is the
// explicit projection time; Cut can later evaluate the same absolute deadlines.
type ImageOptions struct {
	ClusterID  string
	Scheme     *runtime.Scheme
	Validation replication.Validation
	Limits     zonestate.Limits
	MaxZones   int
	Now        time.Time
}

// PreparedImage is a detached primary part of ONE complete candidate. It does
// not publish, acknowledge publication, retain previous results, or read a store.
type PreparedImage struct {
	base               replication.Generation
	cut                Cut
	versions           map[string]zonestate.Version
	desired            []string
	admissionDeadlines map[string]time.Time
	coordinated        map[string]bool
	durable            map[zk.ZoneID]*entry
	status             map[string]PrimaryStatusObservation
}

// Base identifies the complete image from which all members and heads came.
func (p *PreparedImage) Base() replication.Generation { return p.base }

// Cut evaluates only the prepared cut's expiry predicates. This is not a live
// Runtime.Capture. Published is always false, including for active members.
// A desired-only placeholder has zero incarnation, sequence, phase and version;
// it is unavailable coverage, never evidence of durable membership/admission.
func (p *PreparedImage) Cut(now time.Time) Cut {
	c := p.cut
	c.now = now
	return c
}

// Admitted reports exact current-source lifecycle readiness from this image,
// including parent witnesses and absolute primary/ancestor expiry. It is
// independent of Published and does not acknowledge whole-bundle publication.
// Missing members and desired-only placeholders are never admitted. Other
// owners still check their own node-local secondary/stub/RPZ deadlines.
func (p *PreparedImage) Admitted(apex string, now time.Time) bool {
	if p == nil || now.IsZero() {
		return false
	}
	name := canonical(apex)
	e := p.cut.entries[name]
	deadline := p.admissionDeadlines[name]
	return e != nil && p.cut.ready[name] && e.member.Incarnation != 0 && e.member.Phase == zk.PhaseActive && (deadline.IsZero() || now.Before(deadline))
}

// Coordinated reports whether this complete image persists any lifecycle for
// the exact desired source key, including retired/deleted or former-UID sources.
// DNSZone is cluster-scoped: objectName is the exact metadata.name, never a
// namespace/name pair, UID or apex. No normalization or fresh source inference
// occurs. A new UID/apex for this key requires an admitted replacement, rather
// than falling back to uncoordinated legacy membership.
func (p *PreparedImage) Coordinated(objectName string) bool {
	return p != nil && p.coordinated[objectName]
}

// DurableMember returns the actual persisted lifecycle/head before desired
// coverage overlays. It proves durable identity, not readiness/publication.
// id is the exact lifecycle source ZoneID, not the effective RR HeadZone UID.
func (p *PreparedImage) DurableMember(id zk.ZoneID) (Member, bool) {
	if p == nil {
		return Member{}, false
	}
	e := p.durable[id]
	if e == nil {
		return Member{}, false
	}
	m := e.member
	m.Parents = slices.Clone(m.Parents)
	m.Published = false
	return m, true
}

// Versions returns detached durable primary heads, including unavailable or
// retiring members. Desired-only placeholders do not produce version entries.
func (p *PreparedImage) Versions() map[string]zonestate.Version {
	out := make(map[string]zonestate.Version, len(p.versions))
	for apex, v := range p.versions {
		out[apex] = v
	}
	return out
}

// DesiredPrimaryNames identifies all current desired primary apices. Root must
// exclude their legacy desired-data models when composing committed routing.
// Retiring primary coverage may additionally shadow a desired nonprimary model
// until a durable replacement incarnation exists.
func (p *PreparedImage) DesiredPrimaryNames() []string { return slices.Clone(p.desired) }

// PrepareImage validates and prepares one complete immutable primary part.
// All heads (including historical heads) resolve only chunks from this image.
// Valid unreferenced chunks are legal pending GC; malformed ones reject it.
// Incomplete transitions fail closed through coverage, while malformed carriers
// reject the entire candidate. No desired RRsets are compiled into active data.
func PrepareImage(ctx context.Context, image replication.Image, opts ImageOptions) (*PreparedImage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := imageOptions(&opts, image); err != nil {
		return nil, err
	}
	validated, err := replication.Validate(image, opts.Validation)
	if err != nil {
		return nil, err
	}
	owned := validated.Image()
	roster, err := decodeImage(ctx, owned, opts)
	if err != nil {
		return nil, err
	}
	return prepareImageRoster(ctx, owned.Generation(), roster, opts)
}

func prepareImageRoster(ctx context.Context, base replication.Generation, roster imageRoster, opts ImageOptions) (*PreparedImage, error) {
	// Only build is reused: this runtime has no store, repository, callbacks or
	// live capture. All inputs to build are detached and come from this candidate.
	r := Runtime{o: Options{Limits: opts.Limits, Now: func() time.Time { return opts.Now }}}
	entries := make(map[string]*entry, len(roster.ledgers))
	models := make(map[string]*zone.Zone)
	versions := make(map[string]zonestate.Version)
	current := make(map[string]bool)
	coordinated := make(map[string]bool, len(roster.ledgers))
	durable := make(map[zk.ZoneID]*entry, len(roster.ledgers))
	status := make(map[string]PrimaryStatusObservation, len(roster.ledgers))
	for _, l := range roster.ledgers {
		coordinated[l.Spec.Zone.ObjectName] = true
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		z := roster.zones[l.Spec.Zone.ObjectName]
		config := imageCoverageConfig(l, z)
		var view *zonestate.View
		if l.Spec.Type == public.ZoneTypePrimary {
			view = roster.heads[zk.HeadName(l.Spec.HeadZone())]
			if (l.Spec.Phase == zk.PhaseActive && view == nil) || (view != nil && view.Head.Spec.Lifecycle == zk.LifecycleTombstoned && phaseOrder(l.Spec.Phase) < phaseOrder(zk.PhaseRetiring)) || (l.Spec.Phase == zk.PhaseRetired && view != nil && view.Head.Spec.Lifecycle != zk.LifecycleTombstoned) {
				return nil, fmt.Errorf("%w: lifecycle/head phase mismatch", ErrInvalid)
			}
		}
		var raw primaryModelStatus
		e, err := r.buildWithStatus(ctx, l, view, config, &raw)
		if err != nil {
			return nil, err
		}
		if entries[l.Spec.Zone.Apex] != nil {
			return nil, fmt.Errorf("%w: duplicate lifecycle apex", ErrInvalid)
		}
		entries[l.Spec.Zone.Apex] = e
		durable[l.Spec.Zone] = e
		status[l.Spec.Zone.ObjectName] = imageStatus(e, z, raw)
		current[l.Spec.Zone.Apex] = imageCurrentSource(l, z)
		if l.Spec.Type != public.ZoneTypePrimary {
			continue // Other owners supply nonprimary data, never this builder.
		}
		if !current[l.Spec.Zone.Apex] {
			e.model = e.coverage
			if z != nil && z.Spec.Type == public.ZoneTypePrimary && canonical(z.Spec.ZoneName) == l.Spec.Zone.Apex {
				// Source replacement uses CURRENT query ACLs for no-data coverage.
				e.coverage, err = zone.BuildCommittedUnavailable(z)
				if err != nil {
					return nil, err
				}
				e.model = e.coverage
			}
		}
		if imageRemovalComplete(l, z) {
			continue
		}
		models[l.Spec.Zone.Apex] = e.model
		if view != nil {
			versions[l.Spec.Zone.Apex] = version(view)
		}
	}
	// Include the complete admitted roster when assessing late ancestors and
	// nonprimary child boundaries, even though only primary models are returned.
	rosterReady := (Cut{entries: entries}).readiness()
	desired := make([]string, 0)
	for _, z := range roster.zones {
		if z.Spec.Type != public.ZoneTypePrimary {
			continue
		}
		apex := canonical(z.Spec.ZoneName)
		desired = append(desired, apex)
		e := entries[apex]
		if _, ok := models[apex]; ok && e.member.Zone.ObjectName == z.Name && e.member.Zone.UID == string(z.UID) && e.member.Type == public.ZoneTypePrimary {
			continue
		}
		coverage, err := zone.BuildCommittedUnavailable(z)
		if err != nil {
			return nil, err
		}
		// Deliberately no lifecycle/head or invented durable incarnation.
		entries[apex] = &entry{model: coverage, coverage: coverage, member: Member{Zone: zk.ZoneID{ClusterID: opts.ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: apex}, Type: public.ZoneTypePrimary}}
		models[apex] = coverage
		delete(versions, apex)
		current[apex] = false
	}
	if len(models) > opts.MaxZones {
		return nil, ErrCapacity
	}
	ordered := make([]*zone.Zone, 0, len(models))
	for _, model := range models {
		ordered = append(ordered, model)
	}
	snapshot, err := zone.NewSnapshot(base.Revision, true, ordered...)
	if err != nil {
		return nil, err
	}
	c := Cut{snapshot: snapshot, entries: entries, now: opts.Now}
	c.ready = c.readiness()
	for apex := range c.ready {
		c.ready[apex] = c.ready[apex] && rosterReady[apex] && current[apex]
	}
	// A desired late child of any type is already a routing boundary. An
	// admitted parent cannot serve before the child's exact incarnation and
	// witness are present in this same candidate (including before child head).
	for _, child := range roster.zones {
		apex := canonical(child.Spec.ZoneName)
		for _, ancestor := range imageAncestors(apex) {
			parent := entries[ancestor]
			if parent == nil || parent.member.Type != public.ZoneTypePrimary || parent.member.Phase != zk.PhaseActive {
				continue
			}
			childEntry := entries[apex]
			bound := false
			if childEntry != nil && childEntry.lifecycle != nil && imageCurrentSource(childEntry.lifecycle, child) && phaseOrder(childEntry.member.Phase) <= phaseOrder(zk.PhaseActive) {
				for _, p := range childEntry.member.Parents {
					if p.Zone == parent.member.Version.Zone && p.Incarnation == parent.member.Incarnation && p.CommitSequence != 0 && !p.Released && parent.member.Version.CommitSequence >= p.CommitSequence && (parent.member.Version.CommitSequence != p.CommitSequence || parent.member.Version.RootDigest == p.RootDigest) && hasFence(parent.view, apex) {
						bound = true
						break
					}
				}
			}
			if !bound {
				c.ready[ancestor] = false
			}
		}
	}
	// A captured parent with a stale source cannot grant its child's readiness.
	// Ancestors are processed first, so this requires no unbounded fixed point.
	apices := make([]string, 0, len(entries))
	for apex := range entries {
		apices = append(apices, apex)
	}
	slices.SortFunc(apices, func(a, b string) int { return len(a) - len(b) })
	deadlines := make(map[string]time.Time, len(entries))
	for _, apex := range apices {
		deadline := entries[apex].expiry
		for _, parent := range entries[apex].member.Parents {
			if !c.ready[parent.Zone.Apex] {
				c.ready[apex] = false
			}
			if expiry := deadlines[parent.Zone.Apex]; !expiry.IsZero() && (deadline.IsZero() || expiry.Before(deadline)) {
				deadline = expiry
			}
		}
		deadlines[apex] = deadline
	}
	slices.Sort(desired)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	imageStatusDeadlines(status)
	return &PreparedImage{base: base, cut: c, versions: versions, desired: desired, admissionDeadlines: deadlines, coordinated: coordinated, durable: durable, status: status}, nil
}

func imageAncestors(apex string) []string {
	labels := dns.SplitDomainName(apex)
	ancestors := make([]string, 0, len(labels))
	for i := 1; i <= len(labels); i++ {
		ancestors = append(ancestors, strings.Join(labels[i:], ".")+".")
	}
	return ancestors
}

func imageCurrentSource(l *zk.ZoneLifecycle, z *public.DNSZone) bool {
	if z == nil || l.Spec.Source.Removed || string(z.UID) != l.Spec.Zone.UID || canonical(z.Spec.ZoneName) != l.Spec.Zone.Apex || z.Spec.Type != l.Spec.Type {
		return false
	}
	token, err := primaryzone.SourceToken(z)
	return err == nil && token.UID == l.Spec.Source.UID && token.ResourceVersion == l.Spec.Source.ResourceVersion && token.IntentDigest == l.Spec.Source.IntentDigest
}

func imageCoverageConfig(l *zk.ZoneLifecycle, z *public.DNSZone) *public.DNSZone {
	if z != nil && string(z.UID) == l.Spec.Zone.UID && canonical(z.Spec.ZoneName) == l.Spec.Zone.Apex {
		cp := z.DeepCopy()
		cp.Spec.Type = public.ZoneTypePrimary
		return cp
	}
	return &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: l.Spec.Zone.ObjectName, UID: types.UID(l.Spec.Zone.UID)}, Spec: public.DNSZoneSpec{ZoneName: l.Spec.Zone.Apex, Type: public.ZoneTypePrimary}}
}

func imageRemovalComplete(l *zk.ZoneLifecycle, z *public.DNSZone) bool {
	if l.Spec.Phase != zk.PhaseRetired {
		return false
	}
	for _, p := range l.Spec.Parents {
		if !p.Released {
			return false
		}
	}
	// A type replacement at the same apex must await its next incarnation.
	return z == nil || canonical(z.Spec.ZoneName) != l.Spec.Zone.Apex || (z.Spec.Type == public.ZoneTypePrimary && string(z.UID) != l.Spec.Zone.UID)
}
