package enrollment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

// Encode returns bounded canonical complete state. Invitation secrets and
// private member keys are not fields of the persistent document.
func (s *State) Encode() ([]byte, error) {
	if s == nil || validate(s.doc) != nil {
		return nil, ErrInvalid
	}
	d := s.doc
	d.Members = slices.Clone(d.Members)
	d.Invitations = slices.Clone(d.Invitations)
	slices.SortFunc(d.Members, func(a, b memberRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(d.Invitations, func(a, b inviteRecord) int { return strings.Compare(a.ID, b.ID) })
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(raw) > MaxDocumentBytes {
		return nil, ErrCapacity
	}
	return raw, nil
}

// Decode refuses unknown versions, duplicate/unknown fields, noncanonical
// encodings, orphan references and incomplete historical records. It never
// interprets absent bytes as a newly initialized owner.
func Decode(raw []byte) (*State, error) {
	if len(raw) == 0 || len(raw) > MaxDocumentBytes {
		return nil, ErrInvalid
	}
	var d document
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d) != nil {
		return nil, ErrInvalid
	}
	s := &State{doc: d}
	canonical, err := s.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return nil, ErrInvalid
	}
	return s, nil
}

// Digest identifies every owner field, including consumed invitations and
// revocation history. Adapters must use it with the exact original revision.
func (s *State) Digest() (string, error) {
	raw, err := s.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validate(d document) error {
	if d.Version != 1 || !validUUID(d.Cluster) || d.Revision == 0 || d.UpdatedAt.IsZero() || len(d.Members) == 0 || len(d.Members) > MaxMemberHistory || d.Invitations == nil || len(d.Invitations) > MaxInvitations {
		return ErrInvalid
	}
	members := make(map[string]memberRecord, len(d.Members))
	certificates := make(map[string]bool, len(d.Members))
	names, addresses := map[string]bool{}, map[string]bool{}
	active := 0
	for _, m := range d.Members {
		if !validUUID(m.ID) || !validTarget(m.Name, m.Address) {
			return ErrInvalid
		}
		trust, err := memberidentity.ParseTrust(m.ID, m.CA)
		if err != nil {
			return ErrInvalid
		}
		if _, exists := members[m.ID]; exists || certificates[trust.KeyFingerprint()] {
			return ErrInvalid
		}
		members[m.ID], certificates[trust.KeyFingerprint()] = m, true
		if !m.Revoked {
			active++
			if names[m.Name] || addresses[m.Address] {
				return ErrInvalid
			}
			names[m.Name], addresses[m.Address] = true, true
		}
	}
	if active > MaxActiveMembers {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, i := range d.Invitations {
		if !validUUID(i.ID) || seen[i.ID] || len(i.Hash) != sha256.Size || !validIssuer(i.Issuer) || !validTarget(i.Name, i.Address) || i.IssuedAt.IsZero() || !i.IssuedAt.Before(i.ExpiresAt) || i.ExpiresAt.Sub(i.IssuedAt) > MaxInvitationLifetime || i.IssuedAt.After(d.UpdatedAt) {
			return ErrInvalid
		}
		seen[i.ID] = true
		if _, exists := members[i.Issuer.MemberID]; !exists {
			return ErrInvalid
		}
		if i.UsedBy != "" {
			member, exists := members[i.UsedBy]
			if !exists || member.Name != i.Name || member.Address != i.Address {
				return ErrInvalid
			}
		}
	}
	return nil
}

// RetainsHistory rejects removal/rewrite of old member identities, resurrection
// of revoked members or consumed/canceled invitations, and invitation removal
// before its original expiry. It is additional transition validation, not proof
// that a new member was admitted by an authorized writer.
func (s *State) RetainsHistory(old *State) bool {
	if s == nil || old == nil || validate(s.doc) != nil || validate(old.doc) != nil || s.doc.Cluster != old.doc.Cluster || s.doc.Revision < old.doc.Revision || s.doc.UpdatedAt.Before(old.doc.UpdatedAt) {
		return false
	}
	if s.doc.Revision == old.doc.Revision {
		a, ea := s.Encode()
		b, eb := old.Encode()
		return ea == nil && eb == nil && bytes.Equal(a, b)
	}
	for _, before := range old.doc.Members {
		index := slices.IndexFunc(s.doc.Members, func(m memberRecord) bool { return m.ID == before.ID })
		if index < 0 {
			return false
		}
		after := s.doc.Members[index]
		if after.Name != before.Name || after.Address != before.Address || !bytes.Equal(after.CA, before.CA) || before.Revoked && !after.Revoked {
			return false
		}
	}
	for _, before := range old.doc.Invitations {
		index := slices.IndexFunc(s.doc.Invitations, func(i inviteRecord) bool { return i.ID == before.ID })
		if index < 0 {
			if s.doc.UpdatedAt.Before(before.ExpiresAt) {
				return false
			}
			continue
		}
		after := s.doc.Invitations[index]
		if after.Issuer != before.Issuer || after.Name != before.Name || after.Address != before.Address || !bytes.Equal(after.Hash, before.Hash) || !after.IssuedAt.Equal(before.IssuedAt) || !after.ExpiresAt.Equal(before.ExpiresAt) || before.UsedBy != "" && after.UsedBy != before.UsedBy || before.Canceled && !after.Canceled {
			return false
		}
	}
	return true
}
