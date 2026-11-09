package enrollment

import (
	"crypto/subtle"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

// ClusterID exposes the independently delivered scope without granting membership.
func (i Invitation) ClusterID() string { return i.cluster }

// ExpiresAt returns the original absolute expiry without refreshing it.
func (i Invitation) ExpiresAt() time.Time { return i.record.ExpiresAt }

// Member returns the proposal's claimed target. Only Accept/Admit verifies its
// possession and authority; it is useful for attribution after that check.
func (p Proposal) Member() Member { return p.member }

// ExpiresAt is the original proposal expiry. Accept verifies it against the
// invitation before this value can bound a committed acknowledgment.
func (p Proposal) ExpiresAt() time.Time { return p.invitation.record.ExpiresAt }

// CheckLifetime checks only the original invitation and certificate lifetime.
// Joining code must separately retain its original finite operation context.
func (i Invitation) CheckLifetime(local memberidentity.Trust) error {
	now := time.Now()
	if len(i.secret) != 32 || now.Before(i.record.IssuedAt) || !now.Before(i.record.ExpiresAt) || !i.issuerTrust.ValidAt(now) || !local.ValidAt(now) || local.MemberID() == i.record.Issuer.MemberID {
		return ErrRefused
	}
	return nil
}

// CheckAccepted requires an authenticated complete image to retain this exact
// consumed invitation, target CA and original primary. It is an observation,
// never a replacement acceptance or permission to replay a used invitation.
func (i Invitation) CheckAccepted(s *State, local memberidentity.Trust) error {
	if s == nil || s.ClusterID() != i.cluster || i.CheckLifetime(local) != nil {
		return ErrRefused
	}
	found := false
	for _, r := range s.doc.Invitations {
		if r.ID == i.record.ID {
			found = r.UsedBy == local.MemberID() && !r.Canceled && r.Issuer == i.record.Issuer && r.Name == i.record.Name && r.Address == i.record.Address && r.IssuedAt.Equal(i.record.IssuedAt) && r.ExpiresAt.Equal(i.record.ExpiresAt) && subtle.ConstantTimeCompare(r.Hash, inviteHash(i.cluster, i.record, i.secret)) == 1
		}
	}
	primary, target := false, false
	for _, m := range s.Members() {
		if m.Revoked {
			continue
		}
		if m.ID == i.record.Issuer.MemberID {
			primary = m.Trust.Fingerprint() == i.issuerTrust.Fingerprint() && m.Address == i.issuerAddress
		}
		if m.ID == local.MemberID() {
			target = m.Trust.Fingerprint() == local.Fingerprint() && m.Name == i.record.Name && m.Address == i.record.Address
		}
	}
	if !found || !primary || !target {
		return ErrRefused
	}
	return nil
}
