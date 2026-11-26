package sqlite

import (
	"reflect"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// transferRun records that every revision in (from, authority.Installed.Revision]
// of authority's term was committed by a transfer-neutral commit.
type transferRun struct {
	authority replication.Authority
	from      uint64
}

// TransferNeutral reports whether a, the store's in-memory authority, is the
// writable authority of token t with only its installed revision moved
// forward, and every commit since t's revision was transfer-neutral: it
// changed nothing a zone transfer answer, or a transfer refusal, is computed
// from (D419). A zone transfer captured on t's publication may then still be
// sent; every other strict output keeps comparing the exact token. It reads
// no database and grants nothing.
//
// The run is written under the writer lock before the authority of its
// commit is published, so an authority that a caller has read is covered by
// a run only when every commit up to it was noted as neutral. A commit or
// authority change that is not noted here (a replica install, an authority
// transition, a fence) leaves the published authority different from the
// run's, and the answer false.
func (s *Store) TransferNeutral(t GenerationToken, a replication.Authority) bool {
	if s == nil || t.source != s || !t.AdvancedTo(a) {
		return false
	}
	run := s.transfer.Load()
	return run != nil && run.from <= t.authority.Installed.Revision && reflect.DeepEqual(run.authority, a)
}

// noteTransferLocked records the authority a commit is about to publish and
// whether the commit was transfer-neutral. The caller holds wmu and calls it
// after the SQL commit and before publishAuthorityLocked. A neutral commit
// extends the run that ended at the authority published right before it;
// any other commit starts a new, empty run at its own revision.
func (s *Store) noteTransferLocked(a replication.Authority, neutral bool) {
	from := a.Installed.Revision
	if neutral && from > 0 {
		// The commit was applied to the state of the previous revision.
		from--
		if old := s.transfer.Load(); old != nil {
			if published := s.authority.Load(); published != nil && reflect.DeepEqual(*published, old.authority) {
				next := old.authority
				next.Installed.Revision++
				if reflect.DeepEqual(next, a) {
					from = old.from
				}
			}
		}
	}
	s.transfer.Store(&transferRun{authority: a, from: from})
}

// transferNeutral reports whether every mutation of one commit is one of the
// bookkeeping writes that no zone transfer answer is computed from (D419):
//
//   - a ZoneState head whose only change is its notification intent
//     (NotifySequence, NotifySerial) and the commit sequence that metadata
//     commit advances: the NOTIFY acknowledgment. Content, serial, policy
//     and lifecycle are unchanged.
//   - a status write of a primary DNSZone that keeps its name server
//     addresses: the committed-status projection. A primary is served from
//     its committed head and frozen configuration, and a status-only change
//     keeps it serving the same head (imageServingSource, D395).
//   - a ZoneLifecycle of a primary whose only change is the source
//     resourceVersion and the lifecycle sequence: the driver's realignment
//     of the receipt after such a status write, which serves the same head
//     with the same frozen configuration.
//
// Everything else, an empty mutation list included, is not neutral.
func transferNeutral(ms []*mutation) bool {
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !transferNeutralMutation(m) {
			return false
		}
	}
	return true
}

func transferNeutralMutation(m *mutation) bool {
	if m == nil || m.noop || m.typ != store.Modified || m.prev == nil || m.prev.obj == nil || m.obj == nil {
		return false
	}
	switch next := m.obj.(type) {
	case *zk.ZoneState:
		prev, ok := m.prev.obj.(*zk.ZoneState)
		if !ok || m.action == "status" {
			return false
		}
		a, b := prev.DeepCopy(), next.DeepCopy()
		for _, o := range []*zk.ZoneState{a, b} {
			o.Spec.CommitSequence, o.Spec.NotifySequence, o.Spec.NotifySerial = 0, 0, 0
		}
		return sameNeutralMeta(&a.ObjectMeta, &b.ObjectMeta) && a.TypeMeta == b.TypeMeta && a.Spec == b.Spec
	case *public.DNSZone:
		prev, ok := m.prev.obj.(*public.DNSZone)
		if !ok || m.action != "status" || prev.Spec.Type != public.ZoneTypePrimary {
			return false
		}
		return sameNeutralMeta(&prev.ObjectMeta, &next.ObjectMeta) && prev.TypeMeta == next.TypeMeta &&
			equality.Semantic.DeepEqual(prev.Spec, next.Spec) &&
			equality.Semantic.DeepEqual(prev.Status.NameserverAddresses, next.Status.NameserverAddresses)
	case *zk.ZoneLifecycle:
		prev, ok := m.prev.obj.(*zk.ZoneLifecycle)
		if !ok || m.action == "status" || prev.Spec.Type != public.ZoneTypePrimary {
			return false
		}
		a, b := prev.DeepCopy(), next.DeepCopy()
		for _, o := range []*zk.ZoneLifecycle{a, b} {
			o.Spec.Sequence, o.Spec.Source.ResourceVersion = 0, ""
		}
		return sameNeutralMeta(&a.ObjectMeta, &b.ObjectMeta) && a.TypeMeta == b.TypeMeta && equality.Semantic.DeepEqual(a.Spec, b.Spec)
	}
	return false
}

// sameNeutralMeta compares object metadata apart from the write
// bookkeeping a store write changes: the resourceVersion, the generation
// and the field-manager entries.
func sameNeutralMeta(a, b *metav1.ObjectMeta) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	for _, o := range []*metav1.ObjectMeta{x, y} {
		o.ResourceVersion, o.Generation, o.ManagedFields = "", 0, nil
	}
	return equality.Semantic.DeepEqual(x, y)
}
