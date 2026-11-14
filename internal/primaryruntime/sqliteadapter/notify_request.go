package sqliteadapter

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/api/equality"
)

// NotifyIntentResult describes the exact admitted transaction, not delivery or
// publication. Head retains the actual carrier UID/RV even for a no-op.
type NotifyIntentResult struct {
	Head    *zk.ZoneState
	Changed bool
}

// RequestNotifyTx validates the original complete-image evidence and rearms
// only its acknowledged head. The caller MUST retain current raw authorization,
// original writer/HTTP lease, full dependency pins and final deadline checks
// through the enclosing actual Command. This method never opens another command,
// acquires pins, sends packets, substitutes a later head or wakes a worker.
func (a *Adapter) RequestNotifyTx(ctx context.Context, tx *sqlite.CommandTx, o pr.PrimaryStatusObservation) (NotifyIntentResult, error) {
	var out NotifyIntentResult
	if a == nil || ctx == nil || tx == nil || ctx.Err() != nil {
		return out, pr.ErrUnavailable
	}
	f, ok := pr.PublicationFenceFrom(ctx)
	if !ok || !f.Term.Same(tx.Authority()) || f.Incarnation != tx.Incarnation() {
		return out, pr.ErrUnavailable
	}
	if err := notifyObservationBounds(o); err != nil {
		return out, err
	}
	m := o.Member
	l, err := readLifecycle(ctx, tx, zk.LifecycleName(tx.Authority().ClusterID, m.Zone.ObjectName))
	if err != nil {
		return out, err
	}
	if l.ResourceVersion != o.LifecycleResourceVersion || l.Spec.Zone != m.Zone ||
		l.Spec.Type != m.Type || l.Spec.Incarnation != m.Incarnation || l.Spec.Sequence != m.Sequence ||
		l.Spec.Phase != m.Phase || l.Spec.Source != o.Source ||
		!reflect.DeepEqual(l.Spec.Parents, m.Parents) || l.Spec.HeadZone() != o.HeadZone {
		return out, pr.ErrConflict
	}
	z, err := source(ctx, tx, l)
	if err != nil {
		return out, err
	}
	if z == nil {
		return out, pr.ErrConflict
	}
	v, err := a.readHead(ctx, tx, o.HeadZone)
	if err != nil {
		return out, err
	}
	h := v.Head
	if h.Spec.Lifecycle != zk.LifecycleActive || h.ResourceVersion != o.HeadResourceVersion ||
		h.Spec.CommitSequence != m.Version.CommitSequence || h.Spec.RootDigest != m.Version.RootDigest ||
		h.Spec.Serial != o.Serial {
		return out, pr.ErrConflict
	}
	if err := notifySourceMatches(z, v, o); err != nil {
		return out, err
	}
	all, err := a.roster(ctx, tx)
	if err != nil {
		return out, err
	}
	if err := a.membership(ctx, tx, all, l, nil, true); err != nil {
		return out, err
	}
	if (h.Spec.NotifySequence == 0 && h.Spec.NotifySerial != 0) ||
		(h.Spec.NotifySequence != 0 && (h.Spec.NotifySequence > h.Spec.CommitSequence || h.Spec.NotifySerial != h.Spec.Serial)) {
		return out, pr.ErrInvalid
	}
	switch z.Spec.Policy.Notify {
	case "", public.NotifyNone:
		return NotifyIntentResult{Head: h}, nil
	case public.NotifySpecified, public.NotifyZoneNameServers, public.NotifyBoth:
	default:
		return out, pr.ErrInvalid
	}
	if h.Spec.NotifySequence != 0 {
		// Only a correctly framed intent for this exact serial may coalesce.
		return NotifyIntentResult{Head: h}, nil
	}
	p, err := zonestate.PrepareNotify(h, func(d string) (*zk.ZoneChunk, error) {
		var c zk.ZoneChunk
		err := tx.Get(ctx, store.Key{Kind: zk.KindZoneChunk, Name: zk.ChunkName(d)}, &c)
		return &c, err
	}, a.o.Limits)
	if err != nil {
		return out, err
	}
	saved, err := tx.CommitZoneState(ctx, v.Expected, p, a.o.Limits, nil)
	if err != nil {
		return out, err
	}
	return NotifyIntentResult{Head: saved, Changed: true}, nil
}

func notifyObservationBounds(o pr.PrimaryStatusObservation) error {
	m := o.Member
	if len(m.Parents) > zk.MaxParentFences {
		return pr.ErrCapacity
	}
	for _, s := range []string{m.Zone.ClusterID, m.Zone.ObjectName, m.Zone.UID, m.Zone.Apex,
		o.HeadZone.ClusterID, o.HeadZone.ObjectName, o.HeadZone.UID, o.HeadZone.Apex,
		o.LifecycleResourceVersion, o.HeadResourceVersion, o.Source.UID, o.Source.ResourceVersion,
		o.CommittedSource.ID, o.CommittedSource.UID, o.CommittedSource.ResourceVersion} {
		if s == "" || len(s) > 512 || strings.ContainsRune(s, 0) {
			return pr.ErrInvalid
		}
	}
	for _, p := range m.Parents {
		for _, s := range []string{p.Zone.ClusterID, p.Zone.ObjectName, p.Zone.UID, p.Zone.Apex, p.RootDigest, p.BoundaryID} {
			if len(s) > 512 {
				return pr.ErrCapacity
			}
		}
	}
	if !o.ConfigMatchesDesired || o.Source.Removed || m.Type != public.ZoneTypePrimary || m.Phase != zk.PhaseActive ||
		m.Incarnation == 0 || m.Sequence == 0 || m.Version.Zone != o.HeadZone || m.Version.CommitSequence == 0 ||
		len(m.Version.RootDigest) != 64 || len(o.Source.IntentDigest) != 64 || len(o.CommittedSource.IntentDigest) != 64 ||
		!zk.ValidDigest(m.Version.RootDigest) || o.ContentHash == "" || len(o.ContentHash) > 512 || o.RecordCount < 0 ||
		!zk.ValidDigest(o.Source.IntentDigest) || !zk.ValidDigest(o.CommittedSource.IntentDigest) {
		return pr.ErrConflict
	}
	return nil
}

func notifySourceMatches(z *public.DNSZone, v *zonestate.View, o pr.PrimaryStatusObservation) error {
	token, err := primaryzone.SourceToken(z)
	if err != nil {
		return err
	}
	var frozen public.DNSZone
	if json.Unmarshal(v.Aggregate.Config, &frozen) != nil || frozen.Name != z.Name || frozen.Namespace != z.Namespace ||
		string(frozen.UID) != o.HeadZone.UID || !equality.Semantic.DeepEqual(frozen.Spec, z.Spec) ||
		!equality.Semantic.DeepEqual(frozen.Status.NameserverAddresses, z.Status.NameserverAddresses) {
		return pr.ErrConflict
	}
	for _, source := range v.Aggregate.Sources {
		if source.ID != token.ID {
			continue
		}
		e, err := zonestate.DecodeSourceEnvelope(source.Desired)
		if err != nil {
			return err
		}
		committed := zonestate.SourceToken{ID: source.ID, UID: source.UID, ResourceVersion: source.ResourceVersion, IntentDigest: e.IntentDigest}
		if committed != o.CommittedSource || e.State != "present" || e.Projection != "served" ||
			source.UID != token.UID || e.IntentDigest != token.IntentDigest || source.Manager != store.ManagedBy(z) {
			return pr.ErrConflict
		}
		return nil
	}
	return pr.ErrConflict
}
