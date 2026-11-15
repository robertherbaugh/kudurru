package tsigkeys

import (
	"context"
	"crypto/rand"
	"errors"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Management errors contain no credential input or stored payload values.
var (
	ErrIntent       = errors.New("tsigkeys: invalid provisioning intent")
	ErrPrecondition = errors.New("tsigkeys: original key UID/resourceVersion does not match")
	ErrFileKey      = errors.New("tsigkeys: file-backed keys are read-only")
)

// Operation is an explicit credential lifecycle intent, never a generic Apply.
type Operation string

// Supported lifecycle intents. Replace always creates an independent epoch;
// reseal changes only the envelope under an explicitly captured current key.
const (
	Create  Operation = "create"
	Replace Operation = "replace"
	Revoke  Operation = "revoke"
	Reseal  Operation = "reseal"
)

// MutationInput is bounded write-only input. Existing keys require BOTH original
// UID and RV; create requires absence and omits those bindings. Names/algorithms
// may be normalized at preparation, never resolved against a successor source.
type MutationInput struct {
	Operation       Operation
	KeyName         string
	Algorithm       string
	UID             string
	ResourceVersion string
	Secret          []byte
}

// String redacts write-only provisioning input.
func (MutationInput) String() string { return "private TSIG provisioning input" }

// GoString redacts write-only provisioning input.
func (m MutationInput) GoString() string { return m.String() }

// MarshalJSON omits all provisioning input fields.
func (MutationInput) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// Mutation retains one immutable original input/source/sealer and intended row.
// It grants no user, current writer, publication or transaction authority.
type Mutation struct {
	before    *Prepared
	sealer    *Sealer
	operation Operation
	name      string
	row       *kinds.TSIGKey
	metadata  Metadata
}

// String redacts the prepared private mutation.
func (*Mutation) String() string { return "private prepared TSIG mutation" }

// GoString redacts the prepared private mutation.
func (m *Mutation) GoString() string { return m.String() }

// MarshalJSON omits all prepared mutation fields.
func (*Mutation) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// Plan copies/validates input against the original source outside command gates.
// It never reads storage, changes a publication or returns private key material.
func (p *Prepared) Plan(sealer *Sealer, input MutationInput) (*Mutation, error) {
	if p == nil || p.source == nil || p.baseline == nil || sealer == nil || sealer.Identity() != p.EncryptionIdentity() {
		return nil, ErrInvalid
	}
	if len(input.KeyName) > 254 || len(input.Algorithm) > 64 || len(input.UID) > 256 || len(input.ResourceVersion) > 256 || len(input.Secret) > MaxSecretBytes {
		return nil, ErrBounds
	}
	name := kinds.CanonicalName(input.KeyName)
	if name == "" {
		return nil, ErrIntent
	}
	if _, _, ok := p.baseline.Lookup(name); ok {
		return nil, ErrFileKey
	}
	old := p.source.row(name)
	plan := &Mutation{before: p, sealer: sealer, operation: input.Operation, name: name}
	var material *Material
	var err error
	switch input.Operation {
	case Create:
		if input.UID != "" || input.ResourceVersion != "" || old != nil {
			return nil, ErrPrecondition
		}
		if len(p.source.records) >= kinds.MaxKeys {
			return nil, ErrBounds
		}
		algorithm, ok := dnsserver.TSIGAlgorithm(input.Algorithm)
		if !ok {
			return nil, ErrIntent
		}
		material, err = NewMaterial(Owner{ClusterID: p.source.cluster, UID: rand.Text(), KeyName: name, Algorithm: algorithm}, input.Secret)
	case Replace, Revoke, Reseal:
		if old == nil || input.UID == "" || input.ResourceVersion == "" || string(old.UID) != input.UID || old.ResourceVersion != input.ResourceVersion {
			return nil, ErrPrecondition
		}
		plan.metadata = rowMetadata(old)
		if input.Operation == Revoke || input.Operation == Reseal {
			if len(input.Secret) != 0 || input.Algorithm != "" {
				return nil, ErrIntent
			}
			if input.Operation == Revoke {
				return plan, nil
			}
			material, _, err = sealer.Open(old)
		} else {
			algorithm := old.Spec.Algorithm
			if input.Algorithm != "" {
				var ok bool
				algorithm, ok = dnsserver.TSIGAlgorithm(input.Algorithm)
				if !ok {
					return nil, ErrIntent
				}
			}
			material, err = NewMaterial(Owner{ClusterID: p.source.cluster, UID: string(old.UID), KeyName: name, Algorithm: algorithm}, input.Secret)
		}
	default:
		return nil, ErrIntent
	}
	if err != nil {
		return nil, err
	}
	plan.row, err = sealer.Seal(material)
	if err != nil {
		return nil, err
	}
	if old != nil {
		plan.row.ObjectMeta = *old.ObjectMeta.DeepCopy()
	}
	return plan, nil
}

// Stage runs only inside an ALREADY AUTHORIZED actual CommandTx while the root
// retains original proof/writer/provider/host/key/deadline guards. It first checks
// the original durable preimage, stages only its intended effect, validates the
// COMPLETE intended postimage with the original sealer/file baseline, then arms
// a same-tx memory fence. Ignoring any refusal cannot commit unrelated work.
func (m *Mutation) Stage(ctx context.Context, tx *sqlite.CommandTx) (metadata Metadata, err error) {
	defer func() {
		if err != nil && tx != nil {
			failure := err
			if checkErr := tx.RequireCommitCheck(func(context.Context) error { return failure }); checkErr != nil {
				err = checkErr
			}
		}
	}()
	if m == nil || m.before == nil || m.sealer == nil || tx == nil || ctx == nil {
		return Metadata{}, ErrInvalid
	}
	if err := m.before.CheckTx(ctx, tx); err != nil {
		return Metadata{}, err
	}
	if m.operation == Revoke {
		if err := tx.Delete(ctx, store.Key{Kind: kinds.KindTSIGKey, Name: kinds.ObjectName(m.name)}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return Metadata{}, err
		}
		metadata = m.metadata
	} else {
		if m.row == nil {
			return Metadata{}, ErrIntent
		}
		obj, err := tx.Apply(ctx, m.row.DeepCopy(), store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: m.operation == Create, ExpectedResourceVersion: m.metadata.ResourceVersion})
		if err != nil {
			return Metadata{}, err
		}
		row, ok := obj.(*kinds.TSIGKey)
		if !ok {
			return Metadata{}, ErrInvalid
		}
		metadata = rowMetadata(row)
	}
	post, err := ReadTx(ctx, tx)
	if err != nil {
		return Metadata{}, err
	}
	if _, err := post.Prepare(m.sealer, m.before.baseline); err != nil {
		return Metadata{}, err
	}
	if err := post.RequirePostimage(ctx, tx); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

// RequirePostimage must follow typed complete same-tx postimage validation. It
// checks these exact rows again while the tx is active, then bars later actual
// writes to this private kind with a bounded memory-only final journal fence.
// It never relaxes the original preimage/proof/current-writer admission checks.
func (s *Source) RequirePostimage(ctx context.Context, tx *sqlite.CommandTx) error {
	if ctx == nil {
		if tx == nil {
			return ErrInvalid
		}
		return tx.RequireKindUnchanged(ctx, kinds.KindTSIGKey)
	}
	if err := s.CheckTx(ctx, tx); err != nil {
		return err
	}
	return tx.RequireKindUnchanged(ctx, kinds.KindTSIGKey)
}
