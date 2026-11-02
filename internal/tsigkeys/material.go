// Package tsigkeys prepares immutable, owner-bound stored TSIG credentials.
// No reader starts workers, mutates storage or resolves host secret references.
package tsigkeys

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Purpose and bounds apply only to explicitly provisioned stored credentials.
const (
	Purpose           = "stored-tsig"
	MaxSecretBytes    = 512
	maxPlaintextBytes = 4096
)

// Errors describe refusal categories without private field values.
var (
	ErrInvalid = errors.New("tsigkeys: invalid owner or credential")
	ErrProof   = errors.New("tsigkeys: encrypted credential unavailable")
	ErrBounds  = errors.New("tsigkeys: source exceeds bounds")
	ErrSource  = errors.New("tsigkeys: original durable source changed")
)

// Owner binds material to the actual durable object incarnation.
type Owner struct {
	ClusterID string `json:"clusterID"`
	UID       string `json:"uid"`
	KeyName   string `json:"keyName"`
	Algorithm string `json:"algorithm"`
}

func (o Owner) valid() bool {
	algorithm, ok := dnsserver.TSIGAlgorithm(o.Algorithm)
	return kinds.ValidID(o.ClusterID) && kinds.ValidID(o.UID) && o.KeyName != "" &&
		kinds.CanonicalName(o.KeyName) == o.KeyName && ok && algorithm == o.Algorithm
}

// Material holds detached private bytes. Only explicit compilation discloses them.
type Material struct {
	owner  Owner
	epoch  string
	secret []byte
}

func (*Material) String() string { return "private stored TSIG credential" }

// GoString redacts diagnostic formatting.
func (m *Material) GoString() string { return m.String() }

// MarshalJSON redacts implicit serialization.
func (*Material) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// NewMaterial copies explicit write-only provisioning input and creates an epoch.
func NewMaterial(owner Owner, secret []byte) (*Material, error) {
	if !owner.valid() || len(secret) == 0 || len(secret) > MaxSecretBytes {
		return nil, ErrInvalid
	}
	var epoch [16]byte
	_, _ = rand.Read(epoch[:])
	return &Material{owner: owner, epoch: hex.EncodeToString(epoch[:]), secret: bytes.Clone(secret)}, nil
}

// Binding returns safe owner metadata and the independent credential epoch.
func (m *Material) Binding() (Owner, string) {
	if m == nil {
		return Owner{}, ""
	}
	return m.owner, m.epoch
}

// Key returns one detached immutable-provider input, never a live Lookup source.
func (m *Material) Key() dnsserver.TSIGKey {
	if m == nil {
		return dnsserver.TSIGKey{}
	}
	return dnsserver.TSIGKey{Name: m.owner.KeyName, Algorithm: m.owner.Algorithm, Secret: bytes.Clone(m.secret)}
}

// Sealer owns one purpose-separated immutable current/previous encryption cut.
type Sealer struct {
	seal     *session.Sealer
	identity string
}

func (*Sealer) String() string { return "private TSIG encryption owner" }

// GoString redacts diagnostic formatting.
func (s *Sealer) GoString() string { return s.String() }

// MarshalJSON redacts implicit serialization.
func (*Sealer) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// NewSealer copies derived current/previous keys and never loads files or stores.
func NewSealer(current, previous []byte) (*Sealer, error) {
	s, err := session.NewSealer(Purpose, current, previous)
	if err != nil {
		return nil, ErrInvalid
	}
	a, b := sha256.Sum256(current), sha256.Sum256(previous)
	id := sha256.Sum256(append(a[:], b[:]...))
	return &Sealer{seal: s, identity: hex.EncodeToString(id[:])}, nil
}

// Identity names the captured encryption resource, never plaintext material.
func (s *Sealer) Identity() string {
	if s == nil {
		return ""
	}
	return s.identity
}

type payload struct {
	FormatVersion   int    `json:"formatVersion"`
	Owner           Owner  `json:"owner"`
	CredentialEpoch string `json:"credentialEpoch"`
	Secret          []byte `json:"secret"`
}

// Seal preserves a credential epoch across explicit encryption-key resealing.
func (s *Sealer) Seal(m *Material) (*kinds.TSIGKey, error) {
	if s == nil || s.seal == nil || m == nil || !m.owner.valid() || !kinds.ValidEpoch(m.epoch) || len(m.secret) == 0 || len(m.secret) > MaxSecretBytes {
		return nil, ErrInvalid
	}
	raw, err := json.Marshal(payload{FormatVersion: kinds.FormatVersion, Owner: m.owner, CredentialEpoch: m.epoch, Secret: m.secret}) //nolint:gosec // G117: purpose-separated AEAD input only; never logged or serialized to clients.
	if err != nil || len(raw) > maxPlaintextBytes || session.SealedLen(len(raw)) > kinds.MaxSealedBytes {
		return nil, ErrBounds
	}
	defer clear(raw)
	k := &kinds.TSIGKey{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: kinds.KindTSIGKey}, ObjectMeta: metav1.ObjectMeta{Name: kinds.ObjectName(m.owner.KeyName), UID: types.UID(m.owner.UID)}, Spec: kinds.TSIGKeySpec{FormatVersion: kinds.FormatVersion, ClusterID: m.owner.ClusterID, KeyName: m.owner.KeyName, Algorithm: m.owner.Algorithm, CredentialEpoch: m.epoch, Sealed: s.seal.Seal(raw)}}
	return k, k.Validate()
}

// Open validates exact actual carrier ownership and strict authenticated payload.
// Previous-key use is reported for an explicit command, never resealed on reads.
func (s *Sealer) Open(k *kinds.TSIGKey) (*Material, bool, error) {
	if s == nil || s.seal == nil || k.Validate() != nil {
		return nil, false, ErrInvalid
	}
	owner := Owner{ClusterID: k.Spec.ClusterID, UID: string(k.UID), KeyName: k.Spec.KeyName, Algorithm: k.Spec.Algorithm}
	if !owner.valid() {
		return nil, false, ErrInvalid
	}
	raw, previous, err := s.seal.Open(k.Spec.Sealed)
	if err != nil {
		return nil, false, ErrProof
	}
	defer clear(raw)
	var p payload
	if strictDecode(raw, &p, maxPlaintextBytes) != nil || p.FormatVersion != kinds.FormatVersion || p.Owner != owner || p.CredentialEpoch != k.Spec.CredentialEpoch || !kinds.ValidEpoch(p.CredentialEpoch) || len(p.Secret) == 0 || len(p.Secret) > MaxSecretBytes {
		clear(p.Secret)
		return nil, false, ErrProof
	}
	return &Material{owner: owner, epoch: p.CredentialEpoch, secret: p.Secret}, previous, nil
}

func strictDecode(raw []byte, into any, limit int) error {
	if len(raw) == 0 || len(raw) > limit {
		return ErrBounds
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}
