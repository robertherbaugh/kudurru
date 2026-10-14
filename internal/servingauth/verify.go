package servingauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Authenticate checks proof, then refreshes only THIS View's account authority.
// It never records usage, touches sessions or performs a store/network lookup.
func (v *View) Authenticate(ctx context.Context, proof Credential, now time.Time) (auth.Principal, error) {
	if err := ctx.Err(); err != nil {
		return auth.Principal{}, err
	}
	if v == nil || v.released.Load() || now.IsZero() {
		return auth.Principal{}, ErrCredentials
	}
	switch proof.Kind {
	case APIToken:
		return v.verifyToken(proof.Value, now)
	case Cookie:
		sess, err := v.CookieSession(proof.Value, now)
		return sess.Principal, err
	case Password:
		return v.verifyPassword(ctx, proof.Username, proof.Value, now)
	default:
		return auth.Principal{}, ErrUnsupported
	}
}
func (v *View) verifyToken(wire string, now time.Time) (auth.Principal, error) {
	if len(wire) > 512 {
		return auth.Principal{}, ErrCredentials
	}
	id, ok := tokens.Parse(wire)
	want := tokens.Hash("servingauth unknown credential")
	token, known := v.cut.objects[store.Key{Kind: "APIToken", Name: id}].(*internalv1.APIToken)
	if ok && known {
		want = token.Spec.SecretHash
	}
	got := tokens.Hash(wire)
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	if !ok || !known || !match || token.DeletionTimestamp != nil || token.UID == "" || token.CreationTimestamp.IsZero() || token.CreationTimestamp.After(now) || token.Spec.ExpiresAt != nil && !now.Before(token.Spec.ExpiresAt.Time) {
		return auth.Principal{}, ErrCredentials
	}
	owner := token.Spec.Owner
	p := auth.Principal{AccountUID: owner.UID, AuthMethod: auth.MethodToken, TokenID: id, CredentialIssuedAt: token.CreationTimestamp.Time, Scopes: slices.Clone(token.Spec.Scopes), Grants: slices.Clone(token.Spec.Grants), GrantsIssuedAt: token.CreationTimestamp.Time}
	switch owner.Kind {
	case internalv1.OwnerServiceAccount:
		if owner.ID != "sa:"+owner.Namespace+"/"+owner.Name || owner.Provider != "" && owner.Provider != "kudurru" {
			return auth.Principal{}, ErrCredentials
		}
		p.Kind = auth.KindServiceAccount
		p.ID = owner.ID
		p.Name = "serviceaccount:" + owner.Namespace + ":" + owner.Name
		p.Provider = "kudurru"
		p.ServiceAccount = owner.Namespace + "/" + owner.Name
	case internalv1.OwnerUser:
		if owner.ID != "local:"+owner.Name || owner.Namespace != "" || owner.Provider != "" && owner.Provider != "local" {
			return auth.Principal{}, ErrUnsupported
		}
		p.Kind = auth.KindUser
		p.ID = owner.ID
		p.Name = owner.Name
		p.Provider = "local"
	default:
		return auth.Principal{}, ErrCredentials
	}
	return v.LoadPrincipal(p, now)
}
func (v *View) verifyPassword(ctx context.Context, name, password string, now time.Time) (auth.Principal, error) {
	if v.hasher == nil {
		return auth.Principal{}, ErrUnsupported
	}
	if len(name) > 512 || len(password) > users.MaxPasswordLen {
		return auth.Principal{}, ErrCredentials
	}
	user, ok := v.cut.objects[store.Key{Kind: "LocalUser", Name: users.NormalizeUsername(name)}].(*internalv1.LocalUser)
	phc := dummyPasswordHash
	if ok && user.Spec.PasswordHash != "" {
		phc = user.Spec.PasswordHash
	}
	match, err := v.hasher.Verify(ctx, phc, password)
	if err != nil || !match || !ok || inactive(user) || user.Spec.PasswordHash == "" {
		return auth.Principal{}, ErrCredentials
	}
	// TOTP is a separate admitted replay-consumption operation. Never silently
	// accept a password-only proof for an account requiring that second factor.
	if user.Spec.TOTPSecret != "" {
		return auth.Principal{}, ErrUnsupported
	}
	return v.LoadPrincipal(auth.Principal{Kind: auth.KindUser, ID: "local:" + user.Name, Name: user.Name, Provider: "local", AccountUID: string(user.UID), AuthMethod: "password", CredentialIssuedAt: now}, now)
}

// A valid fixed-cost PHC burns the same bounded crypto work for unknown users.
// Matching it can never authenticate: account existence is checked separately.
const dummyPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" //nolint:gosec // G101: dummy verifier never belongs to an account or grants access.

// CookiePayload is the proposed new epoch-bearing owner schema inside the
// existing authenticated envelope. V1 is never fabricated into an enrollment.
type CookiePayload struct {
	FormatVersion int        `json:"formatVersion"`
	SessionID     string     `json:"sessionID"`
	Epoch         uint64     `json:"epoch,string"`
	Account       AccountRef `json:"account"`
	IssuedAt      time.Time  `json:"issuedAt"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	IdleExpiresAt time.Time  `json:"idleExpiresAt"`
	CSRF          string     `json:"csrf"`
}

// CookieFormatVersion is the durable-enrollment envelope payload version.
const CookieFormatVersion = 2

// CSRFHash stores only the verifier of an enrolled cookie's anti-CSRF secret.
func CSRFHash(csrf string) string { s := sha256.Sum256([]byte(csrf)); return hex.EncodeToString(s[:]) }

// SealSession seals only an EXISTING captured enrollment. Root must first commit
// enrollment through the current fenced issuance command; this never creates it.
func (v *View) SealSession(id, csrf string) (string, error) {
	if v == nil || v.released.Load() {
		return "", ErrCredentials
	}
	record, ok := v.sessions[id]
	if !ok || record.Revoked || len(csrf) < 32 || len(csrf) > 256 || !csrfMatches(csrf, record.CSRFHash) {
		return "", ErrCredentials
	}
	payload := CookiePayload{FormatVersion: CookieFormatVersion, SessionID: id, Epoch: record.Epoch, Account: record.Account, IssuedAt: record.IssuedAt, ExpiresAt: record.ExpiresAt, IdleExpiresAt: record.IdleExpiresAt, CSRF: csrf}
	raw, err := json.Marshal(payload)
	if err != nil || session.SealedLen(len(raw))+len(session.DefaultCookieName)+1 > session.MaxCookieBytes {
		return "", ErrBounds
	}
	return v.sealer.Seal(raw), nil
}
func csrfMatches(csrf, want string) bool {
	return subtle.ConstantTimeCompare([]byte(CSRFHash(csrf)), []byte(want)) == 1
}

// CookieSession returns a detached session only after the complete enrolled
// cookie verification used by Authenticate. CSRF admission remains root-owned.
func (v *View) CookieSession(cookie string, now time.Time) (auth.Session, error) {
	if v == nil || v.released.Load() || now.IsZero() {
		return auth.Session{}, ErrCredentials
	}
	if len(cookie) > session.MaxCookieBytes {
		return auth.Session{}, ErrCredentials
	}
	raw, _, err := v.sealer.Open(cookie)
	if err != nil {
		return auth.Session{}, ErrCredentials
	}
	var payload CookiePayload
	if !strictOwner(raw, &payload, session.MaxCookieBytes) || payload.FormatVersion != CookieFormatVersion || !canonicalEpoch(raw) || len(payload.CSRF) < 32 || len(payload.CSRF) > 256 {
		return auth.Session{}, ErrCredentials
	}
	record, ok := v.sessions[payload.SessionID]
	if !ok || record.Revoked || payload.Epoch != record.Epoch || payload.Account != record.Account || !payload.IssuedAt.Equal(record.IssuedAt) || !payload.ExpiresAt.Equal(record.ExpiresAt) || !payload.IdleExpiresAt.Equal(record.IdleExpiresAt) || !csrfMatches(payload.CSRF, record.CSRFHash) || record.IssuedAt.After(now) || !now.Before(record.ExpiresAt) || !now.Before(record.IdleExpiresAt) {
		return auth.Session{}, ErrCredentials
	}
	if floor := v.accounts[record.Account]; !floor.IsZero() && !record.IssuedAt.After(floor) {
		return auth.Session{}, ErrCredentials
	}
	if floor := v.providers[providerKey(record.Provider, record.ProviderUID)]; !floor.IsZero() && !record.IssuedAt.After(floor) {
		return auth.Session{}, ErrCredentials
	}
	for _, g := range record.Grants {
		if floor := v.floors[g]; !floor.IsZero() && !record.IssuedAt.After(floor) {
			return auth.Session{}, ErrCredentials
		}
	}
	obj := accountObject(v.cut, record.Account)
	if user, ok := obj.(*internalv1.LocalUser); ok && user.Status.SessionsValidAfter != nil && !record.IssuedAt.After(user.Status.SessionsValidAfter.Time) {
		return auth.Session{}, ErrCredentials
	}
	p := auth.Principal{AccountUID: record.Account.UID, AuthMethod: auth.MethodCookie, CredentialIssuedAt: record.IssuedAt, GrantsIssuedAt: record.IssuedAt, Grants: slices.Clone(record.Grants), Provider: record.Provider}
	switch record.Account.Kind {
	case "LocalUser":
		p.Kind = auth.KindUser
		p.ID = "local:" + record.Account.Name
		p.Name = record.Account.Name
	case "ServiceAccount":
		p.Kind = auth.KindServiceAccount
		p.ID = "sa:" + record.Account.Namespace + "/" + record.Account.Name
		p.Name = "serviceaccount:" + record.Account.Namespace + ":" + record.Account.Name
		p.ServiceAccount = record.Account.Namespace + "/" + record.Account.Name
	default:
		return auth.Session{}, ErrUnsupported
	}
	p, err = v.LoadPrincipal(p, now)
	if err != nil {
		return auth.Session{}, err
	}
	return auth.Session{ID: record.ID, Principal: p, RolesSnapshot: slices.Clone(record.Grants), IDP: record.Provider, IssuedAt: record.IssuedAt, ExpiresAt: record.ExpiresAt, IdleExpiresAt: record.IdleExpiresAt, CSRF: payload.CSRF}, nil
}
