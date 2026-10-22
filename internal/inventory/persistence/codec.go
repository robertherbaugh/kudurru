package persistence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Encode bounds all input before allocating the complete payload. It preserves
// finite evidence and requires the existing holder's canonical arbitration.
func Encode(state inventory.State, clusterID string, admittedAt time.Time) ([]replication.Entry, error) {
	if err := validContext(clusterID, admittedAt); err != nil {
		return nil, err
	}
	if err := inventory.CheckStateBounds(state, MaxStateBytes); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTooLarge, err)
	}
	if err := checkEvidence(state, admittedAt); err != nil {
		return nil, err
	}
	data, err := inventory.EncodeState(state)
	if err != nil {
		return nil, err
	}
	root := Root{FormatVersion: FormatVersion, ClusterID: clusterID, Generation: state.Generation, UpdatedAt: admittedAt, StateDigest: digest(data), StateBytes: len(data), Chunks: []ChunkRef{}}
	rows := make([]replication.Entry, 0, 1+(len(data)+MaxChunkBytes-1)/MaxChunkBytes)
	seen := map[string]bool{}
	for offset := 0; offset < len(data); offset += MaxChunkBytes {
		part := data[offset:min(offset+MaxChunkBytes, len(data))]
		hash := digest(part)
		root.Chunks = append(root.Chunks, ChunkRef{Digest: hash, Bytes: len(part)})
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
	if _, err := ownerBounds(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Decode requires a complete owner inventory. Foreign runtime rows are ignored;
// unknown inventory keys, orphan/duplicate rows and damaged evidence fail closed.
func Decode(rows []replication.Entry, clusterID string, at time.Time) (*Snapshot, error) {
	if err := validContext(clusterID, at); err != nil {
		return nil, err
	}
	owned, err := ownerBounds(rows)
	if err != nil {
		return nil, err
	}
	var root Root
	var rootBytes []byte
	chunks := map[string]Chunk{}
	for _, row := range owned {
		canonical, err := validateRow(row)
		if err != nil {
			return nil, err
		}
		if row.Key == RootKey {
			if rootBytes != nil {
				return nil, ErrInvalid
			}
			if err := json.Unmarshal(canonical, &root); err != nil {
				return nil, err
			}
			rootBytes = canonical
		} else {
			var chunk Chunk
			if err := json.Unmarshal(canonical, &chunk); err != nil {
				return nil, err
			}
			chunks[chunk.Digest] = chunk
		}
	}
	if rootBytes == nil {
		return nil, ErrUninitialized
	}
	if root.ClusterID != clusterID {
		return nil, replication.ErrWrongCluster
	}
	if root.UpdatedAt.After(at) {
		return nil, ErrInvalid
	}
	// Check every reference and the exact byte sum before payload allocation.
	used := map[string]bool{}
	for _, ref := range root.Chunks {
		c, ok := chunks[ref.Digest]
		if !ok || len(c.Data) != ref.Bytes {
			return nil, ErrInvalid
		}
		used[ref.Digest] = true
	}
	if len(used) != len(chunks) {
		return nil, ErrInvalid
	}
	data := make([]byte, 0, root.StateBytes)
	for _, ref := range root.Chunks {
		data = append(data, chunks[ref.Digest].Data...)
	}
	if len(data) != root.StateBytes || digest(data) != root.StateDigest {
		return nil, ErrInvalid
	}
	state, err := inventory.DecodeState(data)
	if err != nil {
		return nil, fmt.Errorf("%w: state: %v", ErrInvalid, err)
	}
	if state.Generation != root.Generation {
		return nil, ErrInvalid
	}
	if err := checkEvidence(state, root.UpdatedAt); err != nil {
		return nil, err
	}
	return &Snapshot{State: state, Version: Version{Generation: root.Generation, Digest: digest(rootBytes)}, UpdatedAt: root.UpdatedAt}, nil
}

// ValidateEntry validates only this owner's rows; other owners remain independent.
func ValidateEntry(section string, row replication.Entry) error {
	if section != Section || !strings.HasPrefix(row.Key, Prefix) {
		return nil
	}
	_, err := validateRow(row)
	return err
}

// ValidateImage is pure complete-owner validation. Once this owner is enabled,
// a missing root is invalid even for an empty state; legacy gating belongs above.
func ValidateImage(image *replication.Image) error {
	if image == nil {
		return ErrInvalid
	}
	_, err := Decode(image.Payload.Runtime, image.Term.ClusterID, image.CreatedAt)
	return err
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
			return nil, ErrTooLarge
		}
		if len(row.Data) == 0 || seen[row.Key] {
			return nil, ErrInvalid
		}
		seen[row.Key] = true
		total += len(row.Key) + len(row.Data)
		if total > MaxDurableBytes {
			return nil, ErrTooLarge
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
		var root Root
		if err := json.Unmarshal(canonical.Data, &root); err != nil {
			return nil, ErrInvalid
		}
		if root.FormatVersion != FormatVersion || validContext(root.ClusterID, root.UpdatedAt) != nil || root.Generation == math.MaxUint64 || !validDigest(root.StateDigest) || root.StateBytes <= 0 || root.StateBytes > MaxStateBytes || len(root.Chunks) == 0 || len(root.Chunks) > MaxChunks {
			return nil, ErrInvalid
		}
		total := 0
		for i, ref := range root.Chunks {
			if !validDigest(ref.Digest) || ref.Bytes <= 0 || ref.Bytes > MaxChunkBytes || (i < len(root.Chunks)-1 && ref.Bytes != MaxChunkBytes) {
				return nil, ErrInvalid
			}
			total += ref.Bytes
		}
		if total != root.StateBytes {
			return nil, ErrInvalid
		}
		value = root
	case strings.HasPrefix(row.Key, ChunkPrefix):
		var chunk Chunk
		if err := json.Unmarshal(canonical.Data, &chunk); err != nil {
			return nil, ErrInvalid
		}
		if chunk.FormatVersion != FormatVersion || !validDigest(chunk.Digest) || row.Key != ChunkPrefix+chunk.Digest || len(chunk.Data) == 0 || len(chunk.Data) > MaxChunkBytes || digest(chunk.Data) != chunk.Digest {
			return nil, ErrInvalid
		}
		value = chunk
	default:
		return nil, ErrInvalid
	}
	// Typed roundtrip rejects omitted/null fields, casing aliases and unknown
	// fields while allowing insignificant JSON whitespace and key ordering.
	typed, err := encodeRow(row.Key, value)
	if err != nil || !bytes.Equal(typed.Data, canonical.Data) {
		return nil, ErrInvalid
	}
	return canonical.Data, nil
}

func encodeRow(key string, value any) (replication.Entry, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return replication.Entry{}, err
	}
	return replication.ValidateEntry(Section, replication.Entry{Key: key, Data: data}, replication.Limits{MaxObjectBytes: MaxRowBytes, MaxDepth: 8})
}

func validContext(cluster string, at time.Time) error {
	if cluster == "" || len(cluster) > 256 || !utf8.ValidString(cluster) || at.IsZero() {
		return ErrInvalid
	}
	if _, err := at.MarshalJSON(); err != nil {
		return ErrInvalid
	}
	return nil
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Restore is validation, not a repair step. Empty arrays/null are equivalent
// existing encodings; ordering, normalized identities and evidence must agree.
func checkEvidence(s inventory.State, at time.Time) error {
	h, err := inventory.RestoreState(s, func() time.Time { return at })
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	canonical, err := h.ExportState()
	if err != nil {
		return err
	}
	if len(s.Sources) != len(canonical.Sources) || len(s.Watermarks) != len(canonical.Watermarks) {
		return ErrInvalid
	}
	for i, source := range s.Sources {
		other := canonical.Sources[i]
		if source.ID != other.ID || source.Revision != other.Revision || len(source.Clients) != len(other.Clients) {
			return ErrInvalid
		}
		for j, client := range source.Clients {
			normalized := other.Clients[j]
			if len(client.IPs) == 0 {
				client.IPs = nil
			}
			if len(normalized.IPs) == 0 {
				normalized.IPs = nil
			}
			a, err := json.Marshal(client)
			if err != nil {
				return err
			}
			b, err := json.Marshal(normalized)
			if err != nil || !bytes.Equal(a, b) {
				return ErrInvalid
			}
		}
	}
	for i, w := range s.Watermarks {
		other := canonical.Watermarks[i]
		if w.IP != other.IP || w.Source != other.Source || w.ID != other.ID || w.Live != other.Live || !w.ObservedAt.Equal(other.ObservedAt) || !w.ExpiresAt.Equal(other.ExpiresAt) {
			return ErrInvalid
		}
	}
	return nil
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
