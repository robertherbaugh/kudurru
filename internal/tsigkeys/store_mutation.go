package tsigkeys

import (
	"context"
	"errors"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// StoreWriter is the non-transactional driver surface (Kubernetes) that a
// stored TSIG lifecycle command uses. Each write is a single object CAS.
type StoreWriter interface {
	Reader
	Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error)
	Delete(context.Context, store.Key, store.DeleteOptions) error
}

// ErrProvisional reports that a create committed its first phase but could
// not bind the key to its assigned object UID; the provisional carrier was
// removed if possible. The source must be inspected before a new decision.
var ErrProvisional = errors.New("tsigkeys: provisional carrier could not be bound")

// ApplyStore executes a planned mutation on a driver without multi-object
// transactions. The complete original source cut must still be current (one
// read compared by identity); each write is then a single exact-version CAS.
// A Kubernetes object UID is assigned by the API server, so Create stages a
// provisional carrier and re-seals it bound to the assigned UID; if binding
// fails, it deletes exactly that carrier once. The postimage must prepare
// with the original sealer and file baseline. Nothing is retried.
func (m *Mutation) ApplyStore(ctx context.Context, st StoreWriter) (Metadata, error) {
	if m == nil || m.before == nil || m.sealer == nil || st == nil || ctx == nil {
		return Metadata{}, ErrInvalid
	}
	current, err := Read(ctx, st, m.before.source.cluster)
	if err != nil {
		return Metadata{}, err
	}
	if current.Identity() != m.before.source.Identity() {
		return Metadata{}, ErrPrecondition
	}
	key := store.Key{Kind: kinds.KindTSIGKey, Name: kinds.ObjectName(m.name)}
	var metadata Metadata
	switch m.operation {
	case Revoke:
		if err := st.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, ExpectedUID: m.metadata.UID, ExpectedResourceVersion: m.metadata.ResourceVersion}); err != nil {
			return Metadata{}, err
		}
		metadata = m.metadata
	case Replace, Reseal:
		if m.row == nil {
			return Metadata{}, ErrIntent
		}
		obj, err := st.Apply(ctx, m.row.DeepCopy(), store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: m.metadata.ResourceVersion})
		if err != nil {
			return Metadata{}, err
		}
		row, ok := obj.(*kinds.TSIGKey)
		if !ok || string(row.UID) != m.metadata.UID {
			return Metadata{}, ErrInvalid
		}
		metadata = rowMetadata(row)
	case Create:
		if m.row == nil {
			return Metadata{}, ErrIntent
		}
		material, _, err := m.sealer.Open(m.row)
		if err != nil {
			return Metadata{}, err
		}
		defer clear(material.secret)
		provisional := m.row.DeepCopy()
		provisional.UID = ""
		obj, err := st.Apply(ctx, provisional, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		if err != nil {
			return Metadata{}, err
		}
		created, ok := obj.(*kinds.TSIGKey)
		if !ok || created.UID == "" || created.ResourceVersion == "" {
			return Metadata{}, ErrInvalid
		}
		bound, err := m.bind(ctx, st, material, created)
		if err != nil {
			// Remove exactly this provisional carrier, once; never a successor.
			_ = st.Delete(context.WithoutCancel(ctx), key, store.DeleteOptions{Manager: store.ManagerSystem, ExpectedUID: string(created.UID), ExpectedResourceVersion: created.ResourceVersion})
			return Metadata{}, errors.Join(ErrProvisional, err)
		}
		metadata = rowMetadata(bound)
	default:
		return Metadata{}, ErrIntent
	}
	post, err := Read(ctx, st, m.before.source.cluster)
	if err != nil {
		return metadata, err
	}
	if _, err := post.Prepare(m.sealer, m.before.baseline); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func (m *Mutation) bind(ctx context.Context, st StoreWriter, material *Material, created *kinds.TSIGKey) (*kinds.TSIGKey, error) {
	owner := material.owner
	owner.UID = string(created.UID)
	boundMaterial := &Material{owner: owner, epoch: material.epoch, secret: material.secret}
	row, err := m.sealer.Seal(boundMaterial)
	if err != nil {
		return nil, err
	}
	row.ObjectMeta = *created.ObjectMeta.DeepCopy()
	obj, err := st.Apply(ctx, row, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: created.ResourceVersion})
	if err != nil {
		return nil, err
	}
	out, ok := obj.(*kinds.TSIGKey)
	if !ok || out.UID != created.UID {
		return nil, ErrInvalid
	}
	return out, nil
}

// RevokeUnusable deletes one stored carrier that the sealer cannot open (for
// example an interrupted provisional create), bound to its exact observed UID
// and resourceVersion. A carrier that opens must use the planned Revoke.
func RevokeUnusable(ctx context.Context, st StoreWriter, sealer *Sealer, keyName, uid, resourceVersion string) (Metadata, error) {
	if ctx == nil || st == nil || sealer == nil || uid == "" || resourceVersion == "" {
		return Metadata{}, ErrInvalid
	}
	name := kinds.ObjectName(kinds.CanonicalName(keyName))
	if name == "" {
		return Metadata{}, ErrIntent
	}
	list, err := st.List(ctx, kinds.KindTSIGKey, store.ListOptions{Limit: kinds.MaxKeys + 1})
	if err != nil {
		return Metadata{}, err
	}
	for _, obj := range list.Items {
		row, ok := obj.(*kinds.TSIGKey)
		if !ok || row.Name != name {
			continue
		}
		if string(row.UID) != uid || row.ResourceVersion != resourceVersion {
			return Metadata{}, ErrPrecondition
		}
		if _, _, err := sealer.Open(row); err == nil {
			return Metadata{}, ErrPrecondition
		}
		metadata := rowMetadata(row)
		return metadata, st.Delete(ctx, store.Key{Kind: kinds.KindTSIGKey, Name: name}, store.DeleteOptions{Manager: store.ManagerSystem, ExpectedUID: uid, ExpectedResourceVersion: resourceVersion})
	}
	return Metadata{}, ErrPrecondition
}
