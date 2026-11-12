package sqlite

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

const (
	anchorLocalPrefix         = "trustanchors-local/"
	anchorLocalKey            = "trustanchors-local/v1/state"
	anchorLocalProvenance     = "physical-schema-8"
	anchorLocalMaxBytes       = 256 << 10
	anchorLocalMaxRevocations = 256
)

// This private record is never a replication section or a serving credential.
// It is updated in the SAME SQLite transaction as the exact accepted owner.
// A coherent rollback of the entire database cannot be detected by this nonce.
type anchorLocalRecord struct {
	Format         int                     `json:"format"`
	Provenance     string                  `json:"provenance"`
	Carrier        string                  `json:"carrier"`
	Revision       uint64                  `json:"revision,string"`
	Phase          string                  `json:"phase"`
	Member         string                  `json:"member"`
	Cluster        string                  `json:"cluster"`
	History        string                  `json:"history"`
	Domain         string                  `json:"domain"`
	Owner          accepted.Version        `json:"owner"`
	RetiredThrough uint64                  `json:"retiredThrough,string"`
	Denied         []anchorLocalRevocation `json:"denied"`
	Terminal       bool                    `json:"terminal"`
}

type anchorLocalRevocation struct {
	ID         string           `json:"id"`
	FirstOwner accepted.Version `json:"firstOwner"`
}

type anchorLocal struct {
	record anchorLocalRecord
	raw    string
	digest [32]byte
}

func anchorLocalHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func anchorLocalLabel(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func anchorLocalOwner(v accepted.Version, managed bool) bool {
	if !anchorLocalHex(v.Digest, 64) {
		return false
	}
	if !managed {
		return v.Generation == 1 && v.Pure == (accepted.PureVersion{})
	}
	return v.Pure.Generation >= 1 && v.Pure.Generation < math.MaxUint64-1 && v.Generation == v.Pure.Generation+1 && anchorLocalHex(v.Pure.Digest, 64)
}

func validateAnchorLocal(r anchorLocalRecord) error {
	if r.Format != 1 || r.Provenance != anchorLocalProvenance || !anchorLocalHex(r.Carrier, 32) || r.Revision < 1 || r.Denied == nil || len(r.Denied) > anchorLocalMaxRevocations {
		return accepted.ErrInvalid
	}
	if r.Terminal != (r.RetiredThrough == math.MaxUint64) || (r.Revision == math.MaxUint64 && !r.Terminal) {
		return accepted.ErrInvalid
	}
	switch r.Phase {
	case "awaiting":
		if r.Member != "" || r.Cluster != "" || r.History != "" || r.Domain != "" || r.Owner != (accepted.Version{}) || r.RetiredThrough != 0 || len(r.Denied) != 0 || r.Terminal || r.Revision != 1 {
			return accepted.ErrInvalid
		}
	case "dormant", "managed":
		if !anchorLocalLabel(r.Member) || !anchorLocalLabel(r.Cluster) || !anchorLocalHex(r.History, 32) || !anchorLocalOwner(r.Owner, r.Phase == "managed") {
			return accepted.ErrInvalid
		}
		if r.Phase == "dormant" && (r.Domain != "" || r.RetiredThrough != 0 || len(r.Denied) != 0 || r.Terminal) {
			return accepted.ErrInvalid
		}
		if r.Phase == "managed" && (!anchorLocalHex(r.Domain, 64) || r.RetiredThrough < r.Owner.Generation-1) {
			return accepted.ErrInvalid
		}
	default:
		return accepted.ErrInvalid
	}
	for i, d := range r.Denied {
		if !anchorLocalHex(d.ID, 64) || !anchorLocalOwner(d.FirstOwner, true) || d.FirstOwner.Generation > r.Owner.Generation || (i > 0 && r.Denied[i-1].ID >= d.ID) {
			return accepted.ErrInvalid
		}
	}
	return nil
}

func encodeAnchorLocal(r anchorLocalRecord) (*anchorLocal, error) {
	if err := validateAnchorLocal(r); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(data) > anchorLocalMaxBytes {
		return nil, accepted.ErrBounds
	}
	r.Denied = slices.Clone(r.Denied)
	return &anchorLocal{record: r, raw: string(data), digest: sha256.Sum256(data)}, nil
}

func decodeAnchorLocal(raw string) (*anchorLocal, error) {
	if len(raw) == 0 || len(raw) > anchorLocalMaxBytes || !utf8.ValidString(raw) {
		return nil, accepted.ErrBounds
	}
	if err := preflightAnchorLocal(raw); err != nil {
		return nil, err
	}
	var r anchorLocalRecord
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return nil, accepted.ErrInvalid
	}
	value, err := encodeAnchorLocal(r)
	if err != nil {
		return nil, err
	}
	// Canonical exact re-encoding also rejects duplicates, trailing input, null,
	// escaped/repaired Unicode, alternate numbers, omitted fields and key order.
	if !bytes.Equal([]byte(value.raw), []byte(raw)) {
		return nil, accepted.ErrInvalid
	}
	return value, nil
}

func preflightAnchorLocal(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	depth, tokens, entries, arrayDepth := 0, 0, 0, 0
	sawArray := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return accepted.ErrInvalid
		}
		tokens++
		if tokens > 8192 {
			return accepted.ErrBounds
		}
		if arrayDepth != 0 && depth == arrayDepth && token != json.Delim(']') {
			entries++
			if entries > anchorLocalMaxRevocations {
				return accepted.ErrBounds
			}
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '[':
				// The only supported array is the top-level bounded denied inventory.
				// Reject nested/additional arrays BEFORE allocating the typed slice.
				if sawArray || depth != 1 {
					return accepted.ErrBounds
				}
				sawArray = true
				depth++
				arrayDepth = depth
			case ']':
				arrayDepth = 0
				depth--
			case '{':
				depth++
				if depth > 6 {
					return accepted.ErrBounds
				}
			case '}':
				depth--
			}
		case string:
			if len(value) > 512 {
				return accepted.ErrBounds
			}
		}
	}
	if depth != 0 {
		return accepted.ErrInvalid
	}
	return nil
}

func sameAnchorLocal(a, b *anchorLocal) bool {
	return a == nil && b == nil || a != nil && b != nil && a.digest == b.digest && a.raw == b.raw
}

func anchorRevoked(cursor *anchorCursor) ([]string, error) {
	if cursor == nil || cursor.awaiting {
		return nil, accepted.ErrInvalid
	}
	state, err := cursor.owner.State()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, key := range state.Inspect().Keys {
		if key.Status == trustanchors.Revoked {
			ids = append(ids, key.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// anchorLocalForOwner is a bounded pure postimage computation, never a write,
// acceptance or a read-latest merge. The caller supplies the original local CAS.
func anchorLocalForOwner(old *anchorLocal, next *anchorCursor, member string) (*anchorLocal, error) {
	if old == nil {
		return nil, ErrAnchorUnavailable
	}
	r := old.record
	r.Denied = slices.Clone(r.Denied)
	if next.awaiting {
		if r.Phase != "awaiting" {
			return nil, accepted.ErrCAS
		}
		return old, nil
	}
	root := next.owner.Inspect()
	if r.Phase != "awaiting" && (r.Member != member || r.Cluster != root.ClusterID || r.History != root.HistoryID) {
		return nil, accepted.ErrCAS
	}
	if r.Phase == "managed" && (root.Phase != accepted.Managed || r.Domain != hex.EncodeToString(next.mark.DomainID[:])) {
		return nil, accepted.ErrCAS
	}
	if r.Phase == string(root.Phase) && r.Owner == next.owner.Version() {
		return old, nil
	}
	r.Member, r.Cluster, r.History, r.Owner = member, root.ClusterID, root.HistoryID, next.owner.Version()
	r.Phase = string(root.Phase)
	if root.Phase == accepted.Managed {
		r.Domain = hex.EncodeToString(next.mark.DomainID[:])
		r.RetiredThrough = max(r.RetiredThrough, r.Owner.Generation-1)
		ids, err := anchorRevoked(next)
		if err != nil {
			return nil, err
		}
		for _, d := range r.Denied {
			if !slices.Contains(ids, d.ID) {
				return nil, accepted.ErrCAS
			}
		}
		for _, id := range ids {
			if !slices.ContainsFunc(r.Denied, func(d anchorLocalRevocation) bool { return d.ID == id }) {
				if len(r.Denied) == anchorLocalMaxRevocations {
					r.Terminal = true
					r.RetiredThrough = math.MaxUint64
					break
				}
				r.Denied = append(r.Denied, anchorLocalRevocation{ID: id, FirstOwner: r.Owner})
			}
		}
		slices.SortFunc(r.Denied, func(a, b anchorLocalRevocation) int { return strings.Compare(a.ID, b.ID) })
	}
	candidate, err := encodeAnchorLocal(r)
	if err != nil {
		return nil, err
	}
	if candidate.raw == old.raw {
		return old, nil
	}
	return advanceAnchorLocal(old, r)
}

// anchorAdoptionCovers refuses an adoption that would make this member trust a
// key it knows to be revoked (D369): every key its abandoned owner revoked and
// every key it denied locally must be revoked by the adopted owner too. This is
// the same rule anchorLocalForOwner applies inside one term. A refused member
// installs nothing and keeps serving its own last-good branch DNS-only, which
// already revoked the key; it adopts the authoritative branch on the first
// scheduled pull after that branch's writer has recorded the same tombstone.
func anchorAdoptionCovers(local *anchorLocal, before, next *anchorCursor) error {
	if local == nil {
		return ErrAnchorUnavailable
	}
	known, err := anchorRevoked(before)
	if err != nil {
		return err
	}
	revoked, err := anchorRevoked(next)
	if err != nil {
		return err
	}
	var missing []string
	for _, id := range known {
		if !slices.Contains(revoked, id) && !slices.Contains(missing, id) {
			missing = append(missing, id)
		}
	}
	for _, d := range local.record.Denied {
		if !slices.Contains(revoked, d.ID) && !slices.Contains(missing, d.ID) {
			missing = append(missing, d.ID)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return &AnchorRefusal{Reason: AnchorRefusedCarriedRevocation, Keys: missing}
	}
	return nil
}

// anchorLocalForAdoption is the carrier of a member that replaces its managed
// owner with the authoritative branch's owner (D369). It is a bounded pure
// postimage computation like anchorLocalForOwner; the caller supplies the
// original local CAS and has already run anchorAdoption.
//
// The floor is rebased onto the adopted owner, because the abandoned branch
// counted its own generations and its floor may lie at or above the adopted
// one. Nothing this member knows to be revoked is dropped: an adopted owner
// that does not revoke every key the abandoned owner revoked or this member
// denied is refused (anchorAdoptionCovers), and the denial keeps every carried
// key. A terminal carrier, or a denial that no longer fits, stays terminal.
func anchorLocalForAdoption(old *anchorLocal, before, next *anchorCursor, member string) (*anchorLocal, error) {
	if old == nil || !anchorAdopts(before, next) || old.record.Phase != "managed" {
		return nil, ErrAnchorUnavailable
	}
	root := next.owner.Inspect()
	r := old.record
	if root.Phase != accepted.Managed || r.Member != member || r.Cluster != root.ClusterID || r.History != root.HistoryID || r.Domain != hex.EncodeToString(next.mark.DomainID[:]) || r.Owner != before.owner.Version() {
		return nil, accepted.ErrCAS
	}
	if err := anchorAdoptionCovers(old, before, next); err != nil {
		return nil, err
	}
	revoked, err := anchorRevoked(next)
	if err != nil {
		return nil, err
	}
	r.Owner = next.owner.Version()
	r.Denied = slices.Clone(r.Denied)
	for i := range r.Denied {
		// A receipt cannot name a generation above its carrier's owner. The
		// abandoned branch's later receipts are restated at the adopted owner.
		if r.Denied[i].FirstOwner.Generation > r.Owner.Generation {
			r.Denied[i].FirstOwner = r.Owner
		}
	}
	saturated := false
	for _, id := range revoked {
		if slices.ContainsFunc(r.Denied, func(d anchorLocalRevocation) bool { return d.ID == id }) {
			continue
		}
		if len(r.Denied) == anchorLocalMaxRevocations {
			saturated = true
			break
		}
		r.Denied = append(r.Denied, anchorLocalRevocation{ID: id, FirstOwner: r.Owner})
	}
	slices.SortFunc(r.Denied, func(a, b anchorLocalRevocation) int { return strings.Compare(a.ID, b.ID) })
	if saturated || r.Terminal {
		r.Terminal, r.RetiredThrough = true, math.MaxUint64
		return advanceAnchorLocal(old, r)
	}
	r.RetiredThrough = r.Owner.Generation - 1
	return advanceAnchorLocal(old, r)
}

func advanceAnchorLocal(old *anchorLocal, r anchorLocalRecord) (*anchorLocal, error) {
	if old.record.Revision == math.MaxUint64 {
		return nil, ErrAnchorUnavailable
	}
	r.Revision = old.record.Revision + 1
	if r.Revision == math.MaxUint64 {
		r.Terminal = true
		r.RetiredThrough = math.MaxUint64
	}
	return encodeAnchorLocal(r)
}

func anchorLocalDeny(old *anchorLocal, owner *anchorCursor, ids []string) (*anchorLocal, error) {
	if old == nil || old.record.Phase != "managed" || old.record.Owner != owner.owner.Version() || len(ids) == 0 {
		return nil, ErrAnchorUnavailable
	}
	r := old.record
	r.Denied = slices.Clone(r.Denied)
	r.RetiredThrough = max(r.RetiredThrough, r.Owner.Generation)
	for _, id := range ids {
		if !anchorLocalHex(id, 64) {
			return nil, accepted.ErrInvalid
		}
		if slices.ContainsFunc(r.Denied, func(d anchorLocalRevocation) bool { return d.ID == id }) {
			continue
		}
		if len(r.Denied) == anchorLocalMaxRevocations {
			r.Terminal = true
			r.RetiredThrough = math.MaxUint64
			break
		}
		r.Denied = append(r.Denied, anchorLocalRevocation{ID: id, FirstOwner: r.Owner})
	}
	slices.SortFunc(r.Denied, func(a, b anchorLocalRevocation) int { return strings.Compare(a.ID, b.ID) })
	candidate, err := encodeAnchorLocal(r)
	if err != nil {
		return nil, err
	}
	if candidate.raw == old.raw {
		return old, nil
	}
	return advanceAnchorLocal(old, r)
}
