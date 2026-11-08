package accepted

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// NewDormant proposes a never-managed root. Only the explicit sentinel-to-owner
// initialization transaction may install it. This constructor grants nothing.
func NewDormant(cluster, history string, at time.Time) (Snapshot, error) {
	r := Root{FormatVersion: FormatVersion, TrustPoint: ".", ClusterID: cluster, HistoryID: history, Phase: Dormant, Generation: 1, InitializedAt: at, UpdatedAt: at, Chunks: []ChunkRef{}}
	row, err := encodeRow(RootKey, r)
	if err != nil {
		return Snapshot{}, err
	}
	return DecodeRows([]replication.Entry{row}, cluster)
}

// ProposeManaged is structural framing, NOT cryptographic admission. The concrete
// adapter must check the original Bootstrap/Advance evidence and original writer
// and durably CAS this proposal before its checkpoint can become accepted input.
// Ordinary callers must not use this to stage arbitrary next pure states.
func ProposeManaged(current Snapshot, next trustanchors.State, at, until time.Time) (Snapshot, error) {
	if len(next.Inspect().Recoveries) != len(current.state.Inspect().Recoveries) || !next.RetainsRecoveryHistory(current.state) {
		return Snapshot{}, ErrCAS
	}
	return proposeManaged(current, next, at, until)
}

// ProposeRecovery frames one structurally consistent explicit recovery. This
// remains a proposal: its caller must independently authorize/audit the operator
// and verify the original RecoveryObservation before writing and through commit.
func ProposeRecovery(current Snapshot, next trustanchors.State, at, until time.Time) (Snapshot, error) {
	before, err := current.State()
	if err != nil {
		return Snapshot{}, err
	}
	old, doc := before.Inspect(), next.Inspect()
	if !old.Quarantined || doc.Quarantined || !next.RetainsRecoveryHistory(before) || len(doc.Recoveries) != len(old.Recoveries)+1 {
		return Snapshot{}, ErrCAS
	}
	receipt := doc.Recoveries[len(doc.Recoveries)-1]
	if receipt.Generation != doc.Generation || receipt.PreviousDigest != current.root.Pure.Digest || !receipt.At.Equal(at) {
		return Snapshot{}, ErrCAS
	}
	return proposeManaged(current, next, at, until)
}

func proposeManaged(current Snapshot, next trustanchors.State, at, until time.Time) (Snapshot, error) {
	old := current.root
	if old.Generation == 0 || old.Generation >= math.MaxUint64-1 || at.Before(old.UpdatedAt) {
		return Snapshot{}, ErrCAS
	}
	data, err := trustanchors.Encode(next)
	if err != nil {
		return Snapshot{}, err
	}
	doc := next.Inspect()
	if doc.ClusterID != old.ClusterID || (old.Phase == Dormant && doc.Generation != 1) || (old.Phase == Managed && (doc.Generation != old.Pure.Generation+1 || doc.PolicyID != old.PolicyID || doc.SeedDigest != old.SeedDigest)) {
		return Snapshot{}, ErrCAS
	}
	r := Root{FormatVersion: FormatVersion, TrustPoint: ".", ClusterID: old.ClusterID, HistoryID: old.HistoryID, Phase: Managed, Generation: old.Generation + 1, InitializedAt: old.InitializedAt, UpdatedAt: at, PolicyID: doc.PolicyID, SeedDigest: doc.SeedDigest, Pure: PureVersion{Generation: doc.Generation, Digest: digest(data)}, StateBytes: len(data), Chunks: []ChunkRef{}, Previous: current.version, ObservationUntil: until}
	for offset := 0; offset < len(data); offset += MaxChunkBytes {
		part := data[offset:min(offset+MaxChunkBytes, len(data))]
		r.Chunks = append(r.Chunks, ChunkRef{Digest: digest(part), Bytes: len(part)})
	}
	rows, err := rowsFor(r, data)
	if err != nil {
		return Snapshot{}, err
	}
	return DecodeRows(rows, r.ClusterID)
}

// EncodeRows validates a full next owner before any caller writes a row. It
// cannot manufacture original observation evidence or certify a durable command.
func EncodeRows(s Snapshot) ([]replication.Entry, error) {
	var data []byte
	var err error
	if s.root.Phase == Managed {
		data, err = trustanchors.Encode(s.state)
		if err != nil {
			return nil, err
		}
	}
	rows, err := rowsFor(s.root, data)
	if err != nil {
		return nil, err
	}
	next, err := DecodeRows(rows, s.root.ClusterID)
	if err != nil {
		return nil, err
	}
	if next.Version() != s.Version() {
		return nil, ErrInvalid
	}
	return rows, nil
}

func rowsFor(root Root, data []byte) ([]replication.Entry, error) {
	if len(data) > trustanchors.MaxStateBytes {
		return nil, ErrBounds
	}
	rows := make([]replication.Entry, 0, 1+len(root.Chunks))
	seen := map[string]bool{}
	for offset := 0; offset < len(data); offset += MaxChunkBytes {
		part := data[offset:min(offset+MaxChunkBytes, len(data))]
		hash := digest(part)
		if seen[hash] {
			continue
		}
		seen[hash] = true
		row, err := encodeRow(ChunkPrefix+hash, Chunk{FormatVersion: FormatVersion, Digest: hash, Data: part})
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	row, err := encodeRow(RootKey, root)
	if err != nil {
		return nil, err
	}
	rows = append(rows, row)
	slices.SortFunc(rows, func(a, b replication.Entry) int { return strings.Compare(a.Key, b.Key) })
	return rows, nil
}

// DecodeRows rejects missing, duplicate, unknown, orphan, or incomplete owned
// rows. Foreign runtime rows are ignored. Historical expiry is not restamped:
// structural decoding is neither observation verification nor serving admission.
func DecodeRows(rows []replication.Entry, cluster string) (Snapshot, error) {
	owned, err := ownerBounds(rows)
	if err != nil {
		return Snapshot{}, err
	}
	var root Root
	var rootBytes []byte
	chunks := map[string]Chunk{}
	for _, row := range owned {
		canonical, err := validateRow(row)
		if err != nil {
			return Snapshot{}, err
		}
		if row.Key == RootKey {
			if err := json.Unmarshal(canonical, &root); err != nil {
				return Snapshot{}, ErrInvalid
			}
			rootBytes = canonical
		} else {
			var c Chunk
			if err := json.Unmarshal(canonical, &c); err != nil {
				return Snapshot{}, ErrInvalid
			}
			chunks[c.Digest] = c
		}
	}
	if rootBytes == nil {
		return Snapshot{}, ErrMissing
	}
	if root.ClusterID != cluster {
		return Snapshot{}, replication.ErrWrongCluster
	}
	used := map[string]bool{}
	for _, ref := range root.Chunks {
		c, ok := chunks[ref.Digest]
		if !ok || len(c.Data) != ref.Bytes {
			return Snapshot{}, ErrInvalid
		}
		used[ref.Digest] = true
	}
	if len(used) != len(chunks) {
		return Snapshot{}, ErrInvalid
	}
	out := Snapshot{root: root, version: Version{Generation: root.Generation, Digest: digest(rootBytes), Pure: root.Pure}}
	if root.Phase == Dormant {
		return out, nil
	}
	data := make([]byte, 0, root.StateBytes)
	for _, ref := range root.Chunks {
		data = append(data, chunks[ref.Digest].Data...)
	}
	if len(data) != root.StateBytes || digest(data) != root.Pure.Digest {
		return Snapshot{}, ErrInvalid
	}
	state, err := trustanchors.Decode(data)
	if err != nil {
		return Snapshot{}, err
	}
	pureVersion, err := state.Version()
	if err != nil || pureVersion.Generation != root.Pure.Generation || pureVersion.Digest != root.Pure.Digest {
		return Snapshot{}, ErrInvalid
	}
	d := state.Inspect()
	if d.ClusterID != root.ClusterID || d.Generation != root.Pure.Generation || d.PolicyID != root.PolicyID || d.SeedDigest != root.SeedDigest || d.Checkpoint.Wall.After(root.UpdatedAt) || d.Checkpoint.Wall.Before(root.InitializedAt) {
		return Snapshot{}, ErrInvalid
	}
	out.state = state
	return out, nil
}

// IsAwaitingOwner recognizes ONLY the singleton non-granting physical-migration
// sentinel. Normal entry/image validation always rejects it. No migration or
// registration is installed by this package.
func IsAwaitingOwner(rows []replication.Entry) bool {
	owned, err := ownerBounds(rows)
	if err != nil || len(owned) != 1 || owned[0].Key != RootKey {
		return false
	}
	row, err := replication.ValidateEntry(Section, owned[0], replication.Limits{MaxObjectBytes: MaxRootBytes, MaxDepth: 8})
	return err == nil && bytes.Equal(row.Data, []byte(AwaitingOwnerJSON))
}

// ValidateEntry checks only this owner's normal rows, preserving foreign owners.
func ValidateEntry(section string, row replication.Entry) error {
	if section != Section || !strings.HasPrefix(row.Key, Prefix) {
		return nil
	}
	_, err := validateRow(row)
	return err
}

// ValidateImage requires a complete bound normal owner; no missing/sentinel
// fallback is permitted. Shared negotiation/migrations must be mounted later.
func ValidateImage(image *replication.Image) error {
	if image == nil {
		return ErrInvalid
	}
	s, err := DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		return err
	}
	if s.root.UpdatedAt.After(image.CreatedAt) {
		return ErrInvalid
	}
	return nil
}

func ownerBounds(rows []replication.Entry) ([]replication.Entry, error) {
	count, total := 0, 0
	seen := map[string]bool{}
	for _, row := range rows {
		if !strings.HasPrefix(row.Key, Prefix) {
			continue
		}
		count++
		if count > MaxRows || len(row.Key) > 512 || len(row.Data) > MaxRowBytes {
			return nil, ErrBounds
		}
		if len(row.Data) == 0 || seen[row.Key] {
			return nil, ErrInvalid
		}
		seen[row.Key] = true
		total += len(row.Key) + len(row.Data)
		if total > MaxDurableBytes {
			return nil, ErrBounds
		}
	}
	owned := make([]replication.Entry, 0, count)
	for _, row := range rows {
		if strings.HasPrefix(row.Key, Prefix) {
			owned = append(owned, row)
		}
	}
	return owned, nil
}

func validateRow(row replication.Entry) ([]byte, error) {
	if len(row.Data) == 0 || len(row.Data) > MaxRowBytes || !validUnicode(row.Data) {
		return nil, ErrInvalid
	}
	canonical, err := replication.ValidateEntry(Section, row, replication.Limits{MaxObjectBytes: MaxRowBytes, MaxDepth: 8})
	if err != nil {
		return nil, err
	}
	var value any
	switch {
	case row.Key == RootKey:
		if len(row.Data) > MaxRootBytes {
			return nil, ErrBounds
		}
		var r Root
		if json.Unmarshal(canonical.Data, &r) != nil || validateRoot(r) != nil {
			return nil, ErrInvalid
		}
		value = r
	case strings.HasPrefix(row.Key, ChunkPrefix):
		var c Chunk
		if json.Unmarshal(canonical.Data, &c) != nil || c.FormatVersion != FormatVersion || !validDigest(c.Digest) || row.Key != ChunkPrefix+c.Digest || len(c.Data) == 0 || len(c.Data) > MaxChunkBytes || digest(c.Data) != c.Digest {
			return nil, ErrInvalid
		}
		value = c
	default:
		return nil, ErrInvalid
	}
	typed, err := encodeRow(row.Key, value)
	if err != nil || !bytes.Equal(typed.Data, canonical.Data) {
		return nil, ErrInvalid
	}
	return canonical.Data, nil
}

func validateRoot(r Root) error {
	if r.FormatVersion != FormatVersion || r.TrustPoint != "." || !validLabel(r.ClusterID) || !validHistory(r.HistoryID) || r.Generation == 0 || r.Generation == math.MaxUint64 || !validTime(r.InitializedAt) || !validTime(r.UpdatedAt) || r.UpdatedAt.Before(r.InitializedAt) || r.Chunks == nil {
		return ErrInvalid
	}
	switch r.Phase {
	case Dormant:
		if r.Generation != 1 || !r.InitializedAt.Equal(r.UpdatedAt) || r.PolicyID != "" || r.SeedDigest != "" || r.Pure != (PureVersion{}) || r.StateBytes != 0 || len(r.Chunks) != 0 || r.Previous != (Version{}) || !r.ObservationUntil.IsZero() {
			return ErrInvalid
		}
	case Managed:
		if !validLabel(r.PolicyID) || !validDigest(r.SeedDigest) || r.Pure.Generation == 0 || r.Pure.Generation >= math.MaxUint64-1 || r.Generation != r.Pure.Generation+1 || !validDigest(r.Pure.Digest) || r.StateBytes <= 0 || r.StateBytes > trustanchors.MaxStateBytes || len(r.Chunks) == 0 || len(r.Chunks) > MaxChunks || !validTime(r.ObservationUntil) || !r.ObservationUntil.After(r.UpdatedAt) || r.Previous.Generation != r.Generation-1 || !validDigest(r.Previous.Digest) {
			return ErrInvalid
		}
		if r.Generation == 2 {
			if r.Previous.Pure != (PureVersion{}) {
				return ErrInvalid
			}
		} else if r.Previous.Pure.Generation != r.Pure.Generation-1 || !validDigest(r.Previous.Pure.Digest) {
			return ErrInvalid
		}
		total := 0
		for i, ref := range r.Chunks {
			if !validDigest(ref.Digest) || ref.Bytes <= 0 || ref.Bytes > MaxChunkBytes || (i < len(r.Chunks)-1 && ref.Bytes != MaxChunkBytes) {
				return ErrInvalid
			}
			total += ref.Bytes
		}
		if total != r.StateBytes {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func encodeRow(key string, value any) (replication.Entry, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return replication.Entry{}, ErrInvalid
	}
	return replication.ValidateEntry(Section, replication.Entry{Key: key, Data: b}, replication.Limits{MaxObjectBytes: MaxRowBytes, MaxDepth: 8})
}
func digest(b []byte) string     { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validDigest(s string) bool  { return len(s) == 64 && lowerHex(s) }
func validHistory(s string) bool { return len(s) == 32 && lowerHex(s) }
func lowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func validTime(at time.Time) bool {
	return !at.IsZero() && at.Location() == time.UTC && at.Unix() > 0 && at.Unix() < math.MaxUint32
}
func validLabel(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// encoding/json otherwise replaces malformed UTF8 and unpaired escaped UTF16.
// Reject those original bytes before canonicalization can conceal corruption.
func validUnicode(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		i++
		for i < len(data) && data[i] != '"' {
			if data[i] != '\\' {
				i++
				continue
			}
			i++
			if i >= len(data) {
				return false
			}
			if data[i] != 'u' {
				i++
				continue
			}
			if i+4 >= len(data) {
				return false
			}
			n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 5
			if n >= 0xdc00 && n <= 0xdfff {
				return false
			}
			if n >= 0xd800 && n <= 0xdbff {
				if i+5 >= len(data) || data[i] != '\\' || data[i+1] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
	}
	return true
}
