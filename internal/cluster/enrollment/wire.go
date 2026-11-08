package enrollment

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

// Wire bounds limit secret-bearing invitations and possession proposals.
const (
	MaxInvitationBytes = 24 << 10
	MaxProposalBytes   = 16 << 10
	invitationPrefix   = "kudurru-join-v1."
)

type wireInvitation struct {
	Version       int       `json:"version"`
	Cluster       string    `json:"cluster"`
	ID            string    `json:"id"`
	Issuer        Issuer    `json:"issuer"`
	IssuerAddress string    `json:"issuerAddress"`
	IssuerCA      []byte    `json:"issuerCA"`
	Name          string    `json:"name"`
	Address       string    `json:"address"`
	IssuedAt      time.Time `json:"issuedAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Secret        []byte    `json:"secret"`
}

type wireProposal struct {
	Version      int          `json:"version"`
	Cluster      string       `json:"cluster"`
	InvitationID string       `json:"invitationID"`
	Issuer       Issuer       `json:"issuer"`
	IssuedAt     time.Time    `json:"issuedAt"`
	ExpiresAt    time.Time    `json:"expiresAt"`
	Secret       []byte       `json:"secret"`
	Member       memberRecord `json:"member"`
	Signature    []byte       `json:"signature"`
}

// Encode explicitly exports a secret bearer invitation for delivery over the
// original authorized operator channel. Never log or persist these returned
// bytes unsealed. Its embedded server CA is trusted only as far as that delivery
// channel; the joiner must not discover a replacement CA from the destination.
func (i Invitation) Encode() (string, error) {
	if len(i.secret) != 32 || i.issuerTrust.MemberID() != i.record.Issuer.MemberID {
		return "", ErrInvalid
	}
	w := wireInvitation{Version: 1, Cluster: i.cluster, ID: i.record.ID, Issuer: i.record.Issuer, IssuerAddress: i.issuerAddress, IssuerCA: i.issuerTrust.Certificate(), Name: i.record.Name, Address: i.record.Address, IssuedAt: i.record.IssuedAt, ExpiresAt: i.record.ExpiresAt, Secret: i.secret}
	// #nosec G117 -- explicit bearer-invitation export for the original authorized channel; never part of persistent owner state
	raw, err := json.Marshal(w)
	if err != nil {
		return "", ErrInvalid
	}
	out := invitationPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(out) > MaxInvitationBytes {
		return "", ErrCapacity
	}
	return out, nil
}

// ParseInvitation retains the exact bounded operator-delivered target, server
// identity and original absolute expiry. It performs no discovery or clock reset.
func ParseInvitation(encoded string) (Invitation, error) {
	if len(encoded) > MaxInvitationBytes {
		return Invitation{}, ErrInvalid
	}
	body, ok := strings.CutPrefix(encoded, invitationPrefix)
	if !ok {
		return Invitation{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return Invitation{}, ErrInvalid
	}
	var w wireInvitation
	if err := strictWire(raw, &w); err != nil {
		return Invitation{}, err
	}
	now := time.Now()
	if w.Version != 1 || !validUUID(w.Cluster) || !validUUID(w.ID) || !validIssuer(w.Issuer) || !validTarget(w.Name, w.Address) || !validTarget("issuer", w.IssuerAddress) || len(w.Secret) != 32 || now.Before(w.IssuedAt) || !now.Before(w.ExpiresAt) || w.ExpiresAt.Sub(w.IssuedAt) > MaxInvitationLifetime {
		return Invitation{}, ErrRefused
	}
	trust, err := memberidentity.ParseTrust(w.Issuer.MemberID, w.IssuerCA)
	if err != nil || !trust.ValidAt(now) {
		return Invitation{}, ErrRefused
	}
	r := inviteRecord{ID: w.ID, Issuer: w.Issuer, Name: w.Name, Address: w.Address, IssuedAt: w.IssuedAt, ExpiresAt: w.ExpiresAt}
	r.Hash = inviteHash(w.Cluster, r, w.Secret)
	return Invitation{cluster: w.Cluster, record: r, secret: w.Secret, issuerTrust: trust, issuerAddress: w.IssuerAddress}, nil
}

// Target returns the exact reviewed node name/address; neither is identity proof.
func (i Invitation) Target() (string, string) { return i.record.Name, i.record.Address }

// IssuingPeer returns the original term, address and independently delivered CA
// for TLS admission before a joiner transmits its invitation or proposal.
func (i Invitation) IssuingPeer() (Issuer, string, memberidentity.Trust) {
	return i.record.Issuer, i.issuerAddress, i.issuerTrust
}

// Encode explicitly exports the secret proposal for its original verified TLS
// destination. Possession is verified only by the owner's Accept/Admit operation.
func (p Proposal) Encode() ([]byte, error) {
	m, err := recordOf(p.member)
	if err != nil {
		return nil, err
	}
	if len(p.invitation.secret) != 32 || len(p.signature) != ed25519.SignatureSize {
		return nil, ErrInvalid
	}
	w := wireProposal{Version: 1, Cluster: p.invitation.cluster, InvitationID: p.invitation.record.ID, Issuer: p.invitation.record.Issuer, IssuedAt: p.invitation.record.IssuedAt, ExpiresAt: p.invitation.record.ExpiresAt, Secret: p.invitation.secret, Member: m, Signature: p.signature}
	// #nosec G117 -- explicit secret proposal wire encoding for the original verified TLS destination, never persistent owner data
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(raw) > MaxProposalBytes {
		return nil, ErrCapacity
	}
	return raw, nil
}

// DecodeProposal checks bounded canonical framing without granting membership
// or accepting the signature. The original owner must verify it inside admission.
func DecodeProposal(raw []byte) (Proposal, error) {
	if len(raw) == 0 || len(raw) > MaxProposalBytes {
		return Proposal{}, ErrInvalid
	}
	var w wireProposal
	if err := strictWire(raw, &w); err != nil {
		return Proposal{}, err
	}
	if w.Version != 1 || !validUUID(w.Cluster) || !validUUID(w.InvitationID) || !validIssuer(w.Issuer) || len(w.Secret) != 32 || len(w.Signature) != ed25519.SignatureSize || w.Member.Revoked || !validTarget(w.Member.Name, w.Member.Address) {
		return Proposal{}, ErrInvalid
	}
	trust, err := memberidentity.ParseTrust(w.Member.ID, w.Member.CA)
	if err != nil {
		return Proposal{}, ErrInvalid
	}
	if w.IssuedAt.IsZero() || !w.IssuedAt.Before(w.ExpiresAt) || w.ExpiresAt.Sub(w.IssuedAt) > MaxInvitationLifetime {
		return Proposal{}, ErrInvalid
	}
	i := Invitation{cluster: w.Cluster, record: inviteRecord{ID: w.InvitationID, Issuer: w.Issuer, IssuedAt: w.IssuedAt, ExpiresAt: w.ExpiresAt}, secret: w.Secret}
	return Proposal{invitation: i, member: Member{ID: w.Member.ID, Name: w.Member.Name, Address: w.Member.Address, Trust: trust}, signature: w.Signature}, nil
}

func strictWire(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrInvalid
	}
	// Compare only in memory. Secret-bearing canonical bytes never enter logs
	// or owner storage. Exact framing also rejects duplicate/trailing fields.
	canonical, err := json.Marshal(out)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ErrInvalid
	}
	return nil
}
