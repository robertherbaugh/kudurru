package tsigkeys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Reader performs one bounded storage list. Call only during preparation or
// inside an admitted command, never as a DNS secret lookup.
type Reader interface {
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
}

// Source retains a detached encrypted source cut, including actual carrier UID/RV.
// Equality proves only its durable input cut; writer/dependency admission is separate.
type Source struct {
	cluster, identity string
	records           []*kinds.TSIGKey
}

// Identity identifies the exact original rows, never plaintext key material.
func (s *Source) Identity() string {
	if s == nil {
		return ""
	}
	return s.identity
}

// ClusterID is the original storage authority, not a configurable fallback.
func (s *Source) ClusterID() string {
	if s == nil {
		return ""
	}
	return s.cluster
}

// Len returns the bounded key count, including an explicit empty source.
func (s *Source) Len() int {
	if s == nil {
		return 0
	}
	return len(s.records)
}

// Read accepts one bounded complete list and refuses truncated or malformed state.
func Read(ctx context.Context, reader Reader, cluster string) (*Source, error) {
	if ctx == nil || reader == nil || !kinds.ValidID(cluster) {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := reader.List(ctx, kinds.KindTSIGKey, store.ListOptions{Limit: kinds.MaxKeys + 1})
	if err != nil {
		return nil, err
	}
	if len(rows.Items) > kinds.MaxKeys || rows.Continue != "" {
		return nil, ErrBounds
	}
	keys := make([]*kinds.TSIGKey, 0, len(rows.Items))
	for _, row := range rows.Items {
		key, ok := row.(*kinds.TSIGKey)
		if !ok {
			return nil, ErrInvalid
		}
		keys = append(keys, key)
	}
	return fromRecords(ctx, cluster, keys)
}

// ReadTx uses the SAME actual CommandTx, including earlier staged changes.
// Bounds are checked before allocating object payloads; errors poison that tx.
func ReadTx(ctx context.Context, tx *sqlite.CommandTx) (*Source, error) {
	if tx == nil {
		return nil, ErrInvalid
	}
	if err := tx.CheckKindBounds(ctx, kinds.KindTSIGKey, kinds.MaxKeys, kinds.MaxKeys*kinds.MaxObjectBytes); err != nil {
		return nil, err
	}
	return Read(ctx, tx, tx.Authority().ClusterID)
}

// CheckTx refuses coordinator lag, replacement, resealing and delete/recreate.
// It does not acquire another transaction or confer current command authority.
func (s *Source) CheckTx(ctx context.Context, tx *sqlite.CommandTx) (err error) {
	defer func() {
		if err != nil && tx != nil {
			// An ignored owner-check error cannot let unrelated staged work escape.
			failure := err
			if checkErr := tx.RequireCommitCheck(func(context.Context) error { return failure }); checkErr != nil {
				err = checkErr
			}
		}
	}()
	if s == nil || tx == nil || tx.Authority().ClusterID != s.cluster {
		return ErrSource
	}
	current, err := ReadTx(ctx, tx)
	if err != nil {
		return err
	}
	if current.identity != s.identity {
		return ErrSource
	}
	return nil
}

// FromImage prepares only an explicit private manifest from one complete image.
func FromImage(ctx context.Context, image *replication.Image) (*Source, error) {
	if ctx == nil || image == nil || !image.Complete || !kinds.ValidID(image.Term.ClusterID) {
		return nil, ErrInvalid
	}
	declared := -1
	for _, kind := range image.KindManifest {
		if kind.Kind == kinds.KindTSIGKey {
			if declared != -1 || kind.APIVersion != public.GroupVersion.String() || kind.Count > kinds.MaxKeys {
				return nil, ErrInvalid
			}
			declared = int(kind.Count)
		}
	}
	if declared < 0 {
		return nil, ErrSource
	}
	keys := make([]*kinds.TSIGKey, 0, declared)
	for _, raw := range image.Payload.Objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var meta struct{ Kind string }
		if json.Unmarshal(raw, &meta) != nil {
			return nil, ErrInvalid
		}
		if meta.Kind != kinds.KindTSIGKey {
			continue
		}
		if len(keys) >= kinds.MaxKeys {
			return nil, ErrBounds
		}
		var key kinds.TSIGKey
		if strictDecode(raw, &key, kinds.MaxObjectBytes) != nil {
			return nil, ErrInvalid
		}
		keys = append(keys, &key)
	}
	if len(keys) != declared {
		return nil, ErrSource
	}
	return fromRecords(ctx, image.Term.ClusterID, keys)
}

func fromRecords(ctx context.Context, cluster string, records []*kinds.TSIGKey) (*Source, error) {
	if !kinds.ValidID(cluster) || len(records) > kinds.MaxKeys {
		return nil, ErrInvalid
	}
	s := &Source{cluster: cluster, records: make([]*kinds.TSIGKey, 0, len(records))}
	for _, key := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if key.Validate() != nil || key.Spec.ClusterID != cluster || !kinds.ValidID(string(key.UID)) || key.ResourceVersion == "" || len(key.ResourceVersion) > 256 || key.Generation < 1 || key.CreationTimestamp.IsZero() || key.DeletionTimestamp != nil || key.APIVersion != public.GroupVersion.String() || key.Kind != kinds.KindTSIGKey {
			return nil, ErrInvalid
		}
		raw, err := json.Marshal(key)
		if err != nil || len(raw) > kinds.MaxObjectBytes {
			return nil, ErrBounds
		}
		s.records = append(s.records, key.DeepCopy())
	}
	slices.SortFunc(s.records, func(a, b *kinds.TSIGKey) int { return strings.Compare(a.Name, b.Name) })
	for i := 1; i < len(s.records); i++ {
		if s.records[i-1].Name == s.records[i].Name {
			return nil, ErrInvalid
		}
	}
	raw, err := json.Marshal(struct {
		Cluster string
		Keys    []*kinds.TSIGKey
	}{cluster, s.records})
	if err != nil {
		return nil, ErrInvalid
	}
	digest := sha256.Sum256(append([]byte("kudurru/tsig/source/v1\x00"), raw...))
	s.identity = hex.EncodeToString(digest[:])
	return s, nil
}

// Open prepares ALL immutable credentials with one captured encryption owner.
// An unavailable stored credential rejects the entire source, never file fallback.
func (s *Source) Open(sealer *Sealer) ([]*Material, bool, error) {
	if s == nil || sealer == nil {
		return nil, false, ErrInvalid
	}
	keys := make([]*Material, 0, len(s.records))
	usedPrevious := false
	for _, record := range s.records {
		material, previous, err := sealer.Open(record)
		if err != nil {
			return nil, false, err
		}
		keys = append(keys, material)
		usedPrevious = usedPrevious || previous
	}
	return keys, usedPrevious, nil
}
