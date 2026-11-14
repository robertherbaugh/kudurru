// Package enrollment owns bounded immutable member and invitation state. Pure
// transitions do not grant writer/operator authority or persist themselves: the
// adapter must retain original authorization, exact state CAS and commit time.
package enrollment

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

// Owner bounds are explicit refusals, never permission to prune member history.
const (
	MaxActiveMembers      = 64
	MaxMemberHistory      = 256
	MaxInvitations        = 64
	MaxDocumentBytes      = 512 << 10
	MaxInvitationLifetime = 10 * time.Minute
)

// Errors reveal no invitation secret, private identity or credential material.
var (
	ErrInvalid  = errors.New("enrollment: invalid complete owner")
	ErrRefused  = errors.New("enrollment: invitation or member refused")
	ErrCapacity = errors.New("enrollment: owner capacity exhausted")
)

// Issuer is the exact original writer term. Adapters derive it from the actual
// admitted transaction, never a peer claim or an operator-supplied timestamp.
type Issuer struct {
	MemberID    string `json:"memberID"`
	Epoch       uint64 `json:"epoch,string"`
	PromotionID string `json:"promotionID"`
}

// Member identifies a node by independently verified public trust, not name or
// address. A revoked UUID/CA is permanent history and cannot be enrolled again.
type Member struct {
	ID, Name, Address string
	Trust             memberidentity.Trust
	Revoked           bool
}

type memberRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	CA      []byte `json:"ca"`
	Revoked bool   `json:"revoked"`
}

type inviteRecord struct {
	ID        string    `json:"id"`
	Hash      []byte    `json:"hash"`
	Issuer    Issuer    `json:"issuer"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedBy    string    `json:"usedBy,omitempty"`
	Canceled  bool      `json:"canceled,omitempty"`
}

type document struct {
	Version     int            `json:"version"`
	Cluster     string         `json:"cluster"`
	Revision    uint64         `json:"revision,string"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	Members     []memberRecord `json:"members"`
	Invitations []inviteRecord `json:"invitations"`
}

// State is immutable. It can be constructed only by validation or a checked
// transition; returned public certificates and diagnostic slices are detached.
type State struct{ doc document }

// Invitation contains the secret only in memory. Public encoding/transport and
// durable response dedup must be explicitly sealed by the caller; State stores
// only its purpose-separated hash. Generic JSON encoding exposes no secret.
type Invitation struct {
	cluster       string
	record        inviteRecord
	secret        []byte
	issuerTrust   memberidentity.Trust
	issuerAddress string
}

// Proposal carries a signed key-possession statement for an exact invitation and
// target. Decoding is structural; Accept verifies possession and admission.
type Proposal struct {
	invitation Invitation
	member     Member
	signature  []byte
}

func validUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

func validTarget(name, address string) bool {
	if len(name) == 0 || len(name) > 63 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || name[0] == '-' || name[len(name)-1] == '-' || len(address) > 320 || strings.ContainsAny(address, "\\/?#@\x00\r\n\t ") {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	n, parseErr := strconv.Atoi(port)
	return err == nil && host != "" && parseErr == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port
}

func validIssuer(i Issuer) bool {
	return validUUID(i.MemberID) && i.Epoch > 0 && validUUID(i.PromotionID)
}

func recordOf(m Member) (memberRecord, error) {
	if !validUUID(m.ID) || m.Trust.MemberID() != m.ID || !validTarget(m.Name, m.Address) {
		return memberRecord{}, ErrInvalid
	}
	return memberRecord{ID: m.ID, Name: m.Name, Address: m.Address, CA: m.Trust.Certificate(), Revoked: m.Revoked}, nil
}

// Initialize explicitly creates one complete owner. Persistence must refuse an
// existing or missing-but-previously-initialized owner instead of calling this.
func Initialize(cluster string, first Member, now time.Time) (*State, error) {
	r, err := recordOf(first)
	if err != nil || first.Revoked || !first.Trust.ValidAt(now) || !validUUID(cluster) || now.IsZero() {
		return nil, ErrInvalid
	}
	return &State{doc: document{Version: 1, Cluster: cluster, Revision: 1, UpdatedAt: now.UTC(), Members: []memberRecord{r}, Invitations: []inviteRecord{}}}, nil
}

// ClusterID identifies the owner cluster for durable CAS.
func (s *State) ClusterID() string {
	if s == nil {
		return ""
	}
	return s.doc.Cluster
}

// Revision identifies this exact owner transition for durable CAS.
func (s *State) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.doc.Revision
}

// UpdatedAt is the original last transition time; loading never refreshes it.
func (s *State) UpdatedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.doc.UpdatedAt
}

// Members returns detached complete history, including permanent revocations.
func (s *State) Members() []Member {
	if s == nil {
		return nil
	}
	out := make([]Member, 0, len(s.doc.Members))
	for _, r := range s.doc.Members {
		trust, _ := memberidentity.ParseTrust(r.ID, r.CA) // State construction already validated it.
		out = append(out, Member{ID: r.ID, Name: r.Name, Address: r.Address, Trust: trust, Revoked: r.Revoked})
	}
	return out
}

func (s *State) active(id string) bool {
	return slices.ContainsFunc(s.doc.Members, func(m memberRecord) bool { return m.ID == id && !m.Revoked })
}

func (s *State) next(now time.Time) (*State, error) {
	if s == nil || now.IsZero() || now.Before(s.doc.UpdatedAt) {
		return nil, ErrInvalid
	}
	if s.doc.Revision == math.MaxUint64 {
		return nil, ErrCapacity
	}
	d := s.doc
	d.Members = slices.Clone(d.Members)
	d.Invitations = slices.Clone(d.Invitations)
	d.Revision++
	d.UpdatedAt = now.UTC()
	return &State{doc: d}, nil
}

func inviteHash(cluster string, r inviteRecord, secret []byte) []byte {
	// Fixed-length secret and UUID fields with separators avoid ambiguous input.
	encoded, _ := json.Marshal(struct {
		Cluster, ID string
		Issuer      Issuer
	}{cluster, r.ID, r.Issuer})
	h := sha256.New()
	_, _ = h.Write([]byte("kudurru/member-invitation/v1\x00"))
	_, _ = h.Write(encoded)
	_, _ = h.Write(secret)
	return h.Sum(nil)
}

// Issue binds one invitation to the original issuer and exact reviewed target.
// Expired invitations are removed only by explicit PruneInvitations, not issue.
func (s *State) Issue(issuer Issuer, name, address string, now, expires time.Time) (*State, Invitation, error) {
	if s == nil || !validIssuer(issuer) || !s.active(issuer.MemberID) || !validTarget(name, address) || !now.Before(expires) || expires.Sub(now) > MaxInvitationLifetime {
		return nil, Invitation{}, ErrRefused
	}
	if len(s.doc.Invitations) >= MaxInvitations {
		return nil, Invitation{}, ErrCapacity
	}
	for _, m := range s.doc.Members {
		if !m.Revoked && (m.Name == name || m.Address == address) {
			return nil, Invitation{}, ErrRefused
		}
	}
	next, err := s.next(now)
	if err != nil {
		return nil, Invitation{}, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, Invitation{}, err
	}
	r := inviteRecord{ID: uuid.NewString(), Issuer: issuer, Name: name, Address: address, IssuedAt: now.UTC(), ExpiresAt: expires.UTC()}
	if slices.ContainsFunc(s.doc.Invitations, func(old inviteRecord) bool { return old.ID == r.ID }) {
		return nil, Invitation{}, ErrRefused
	}
	r.Hash = inviteHash(s.doc.Cluster, r, secret)
	next.doc.Invitations = append(next.doc.Invitations, r)
	var originalIssuer Member
	for _, m := range s.Members() {
		if m.ID == issuer.MemberID {
			originalIssuer = m
			break
		}
	}
	if !originalIssuer.Trust.ValidAt(now) {
		return nil, Invitation{}, ErrRefused
	}
	return next, Invitation{cluster: s.doc.Cluster, record: r, secret: secret, issuerTrust: originalIssuer.Trust, issuerAddress: originalIssuer.Address}, nil
}

func statement(i Invitation, m Member) []byte {
	digest := sha256.Sum256(i.secret)
	raw, _ := json.Marshal(struct {
		Cluster, InvitationID string
		Issuer                Issuer
		IssuedAt, ExpiresAt   time.Time
		SecretHash            string
		ID, Name, Address, CA string
	}{
		i.cluster, i.record.ID, i.record.Issuer, i.record.IssuedAt, i.record.ExpiresAt, hex.EncodeToString(digest[:]), m.ID, m.Name, m.Address, m.Trust.Fingerprint(),
	})
	return raw
}

// Propose proves possession of the local member key for this original target.
// Invitation expiry and current issuer are checked again by Accept at commit.
func (i Invitation) Propose(identity *memberidentity.Identity) (Proposal, error) {
	if identity == nil || len(i.secret) != 32 || !validUUID(i.cluster) {
		return Proposal{}, ErrRefused
	}
	m := Member{ID: identity.Trust().MemberID(), Name: i.record.Name, Address: i.record.Address, Trust: identity.Trust()}
	signature, err := identity.SignEnrollment(statement(i, m))
	if err != nil {
		return Proposal{}, ErrRefused
	}
	return Proposal{invitation: i, member: m, signature: signature}, nil
}

// Accept consumes once and adds a proved member in one new complete state.
// The adapter must stage both effects atomically and retain the original proof
// and actual issuer/time through its final commit check. No member is recaptured.
func (s *State) Accept(proposal Proposal, issuer Issuer, now time.Time) (*State, error) {
	if s == nil || !validIssuer(issuer) || !s.active(issuer.MemberID) || proposal.invitation.cluster != s.doc.Cluster {
		return nil, ErrRefused
	}
	index := slices.IndexFunc(s.doc.Invitations, func(r inviteRecord) bool { return r.ID == proposal.invitation.record.ID })
	if index < 0 {
		return nil, ErrRefused
	}
	r := s.doc.Invitations[index]
	m := proposal.member
	if !proposal.invitation.record.IssuedAt.Equal(r.IssuedAt) || !proposal.invitation.record.ExpiresAt.Equal(r.ExpiresAt) {
		return nil, ErrRefused
	}
	if r.Issuer != issuer || proposal.invitation.record.Issuer != r.Issuer || r.Canceled || r.UsedBy != "" || now.Before(r.IssuedAt) || !now.Before(r.ExpiresAt) || len(proposal.invitation.secret) != 32 || subtle.ConstantTimeCompare(r.Hash, inviteHash(s.doc.Cluster, r, proposal.invitation.secret)) != 1 || m.Name != r.Name || m.Address != r.Address || m.Revoked || !m.Trust.ValidAt(now) || m.Trust.VerifyEnrollment(statement(proposal.invitation, m), proposal.signature) != nil {
		return nil, ErrRefused
	}
	member, err := recordOf(m)
	if err != nil {
		return nil, ErrRefused
	}
	active := 0
	for _, old := range s.doc.Members {
		oldTrust, err := memberidentity.ParseTrust(old.ID, old.CA)
		if err != nil || old.ID == m.ID || oldTrust.KeyFingerprint() == m.Trust.KeyFingerprint() || !old.Revoked && (old.Name == m.Name || old.Address == m.Address) {
			return nil, ErrRefused
		}
		if old.ID == issuer.MemberID && !oldTrust.ValidAt(now) {
			return nil, ErrRefused
		}
		if !old.Revoked {
			active++
		}
	}
	if active >= MaxActiveMembers || len(s.doc.Members) >= MaxMemberHistory {
		return nil, ErrCapacity
	}
	next, err := s.next(now)
	if err != nil {
		return nil, err
	}
	next.doc.Invitations[index].UsedBy = m.ID
	next.doc.Members = append(next.doc.Members, member)
	return next, nil
}

// Revoke retains the exact member identity permanently; it cannot reactivate.
func (s *State) Revoke(id string, now time.Time) (*State, error) {
	if s == nil {
		return nil, ErrInvalid
	}
	index := slices.IndexFunc(s.doc.Members, func(m memberRecord) bool { return m.ID == id })
	if index < 0 {
		return nil, ErrRefused
	}
	next, err := s.next(now)
	if err != nil {
		return nil, err
	}
	if s.doc.Members[index].Revoked {
		return s, nil
	}
	next.doc.Members[index].Revoked = true
	// Old outstanding invitations for this target cannot restore access under
	// a fresh key after revocation. A later reviewed issue is a new operation.
	for j, invite := range next.doc.Invitations {
		if invite.UsedBy == "" && (invite.Name == next.doc.Members[index].Name || invite.Address == next.doc.Members[index].Address) {
			next.doc.Invitations[j].Canceled = true
		}
	}
	return next, nil
}

// PruneInvitations removes only records whose original absolute lifetime ended.
// Removed tokens remain absent even after clock rollback; members are untouched.
func (s *State) PruneInvitations(now time.Time) (*State, error) {
	next, err := s.next(now)
	if err != nil {
		return nil, err
	}
	next.doc.Invitations = slices.DeleteFunc(next.doc.Invitations, func(r inviteRecord) bool { return !now.Before(r.ExpiresAt) })
	if len(next.doc.Invitations) == len(s.doc.Invitations) {
		return s, nil
	}
	return next, nil
}
