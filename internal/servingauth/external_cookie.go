package servingauth

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// ExternalCookieFormatVersion keeps external enrollment distinct from local v2.
const ExternalCookieFormatVersion = 3

func sessionBinding(s ExternalSessionRecord) oidccore.EvidenceBinding {
	return oidccore.EvidenceBinding{Kind: oidccore.SessionEvidence, Provider: s.Provider.ID, ProviderUID: s.Provider.UID, Origin: string(s.Provider.Origin), Issuer: s.Issuer, Subject: s.Subject, IdentityUID: s.Account.UID, RecordID: s.ID}
}
func (v *View) externalSessionPrincipal(ctx context.Context, record ExternalSessionRecord, now time.Time) (auth.Principal, error) {
	if record.Revoked || record.IssuedAt.After(now) || !now.Before(record.ExpiresAt) || !now.Before(record.IdleExpiresAt) {
		return auth.Principal{}, ErrCredentials
	}
	provider, err := v.externalProvider(record.Provider.ID)
	if err != nil || record.Provider != provider.descriptor.Ref || record.Issuer != provider.descriptor.Issuer {
		return auth.Principal{}, ErrCredentials
	}
	i, ok := v.externalIdentities[record.Account]
	if !ok || i.Disabled || i.CreatedAt.After(now) || i.GroupsVerifiedAt != nil && i.GroupsVerifiedAt.After(now) {
		return auth.Principal{}, ErrCredentials
	}
	if err = v.externalFloors(provider, &i, record.Grants, record.IssuedAt); err != nil {
		return auth.Principal{}, err
	}
	evidence, err := v.evidenceKeys.Open(sessionBinding(record), record.Evidence)
	if err != nil || evidence.VerifiedAt().After(record.IssuedAt) {
		return auth.Principal{}, ErrCredentials
	}
	facts := evidence.Facts()
	if facts.IssuedAt.Before(provider.descriptor.CreatedAt) {
		return auth.Principal{}, ErrCredentials
	}
	if floor := v.providers[providerKey(record.Provider.ID, record.Provider.UID)]; !floor.IsZero() && !facts.IssuedAt.After(floor) {
		return auth.Principal{}, ErrCredentials
	}
	identity, err := provider.policy.EvaluateEnrollment(ctx, evidence, now)
	if err != nil {
		return auth.Principal{}, ErrCredentials
	}
	current, err := v.externalGrants(provider, identity, &i, true)
	if err != nil {
		return auth.Principal{}, err
	}
	grants := intersect(record.Grants, current)
	groups := identity.Groups
	if provider.descriptor.Spec.Provisioning.SyncGroups && i.GroupsVerifiedAt != nil {
		groups = intersect(groups, i.Groups)
	}
	return detachedPrincipal(auth.Principal{Kind: auth.KindUser, ID: identity.ID(), Name: identity.Username, Groups: groups, Provider: identity.Provider, AccountUID: i.UID, AuthMethod: auth.MethodCookie, Grants: grants, GrantsIssuedAt: record.IssuedAt, CredentialIssuedAt: record.IssuedAt, External: &auth.ExternalProof{ProviderUID: record.Provider.UID, Origin: string(record.Provider.Origin), SessionID: record.ID}}), nil
}
func (v *View) sealExternalSession(record ExternalSessionRecord, csrf string) (string, error) {
	if record.Revoked || len(csrf) < 32 || len(csrf) > 256 || !csrfMatches(csrf, record.CSRFHash) {
		return "", ErrCredentials
	}
	payload := CookiePayload{FormatVersion: ExternalCookieFormatVersion, SessionID: record.ID, Epoch: record.Epoch, Account: record.Account, IssuedAt: record.IssuedAt, ExpiresAt: record.ExpiresAt, IdleExpiresAt: record.IdleExpiresAt, CSRF: csrf}
	raw, err := json.Marshal(payload)
	if err != nil || session.SealedLen(len(raw))+len(session.DefaultCookieName)+1 > session.MaxCookieBytes {
		return "", ErrBounds
	}
	return v.sealer.Seal(raw), nil
}
func (v *View) externalCookie(raw []byte, now time.Time) (auth.Session, error) {
	var payload CookiePayload
	if !strictOwner(raw, &payload, session.MaxCookieBytes) || !canonicalEpoch(raw) || payload.FormatVersion != ExternalCookieFormatVersion || payload.Account.Kind != "ExternalIdentity" || len(payload.CSRF) < 32 || len(payload.CSRF) > 256 {
		return auth.Session{}, ErrCredentials
	}
	record, ok := v.externalSessions[payload.SessionID]
	if !ok || payload.Epoch != record.Epoch || payload.Account != record.Account || !payload.IssuedAt.Equal(record.IssuedAt) || !payload.ExpiresAt.Equal(record.ExpiresAt) || !payload.IdleExpiresAt.Equal(record.IdleExpiresAt) || !csrfMatches(payload.CSRF, record.CSRFHash) {
		return auth.Session{}, ErrCredentials
	}
	p, err := v.externalSessionPrincipal(context.Background(), record, now)
	if err != nil {
		return auth.Session{}, err
	}
	return auth.Session{ID: record.ID, Principal: p, RolesSnapshot: slices.Clone(p.Grants), IDP: record.Provider.ID, IssuedAt: record.IssuedAt, ExpiresAt: record.ExpiresAt, IdleExpiresAt: record.IdleExpiresAt, CSRF: payload.CSRF}, nil
}

type externalTokenEvidence struct {
	FormatVersion int         `json:"formatVersion"`
	TokenID       string      `json:"tokenID"`
	TokenUID      string      `json:"tokenUID"`
	AccountUID    string      `json:"accountUID"`
	Provider      ProviderRef `json:"provider"`
	IssuedAt      time.Time   `json:"issuedAt"`
}

func (v *View) verifyExternalToken(token *internalv1.APIToken, now time.Time) (auth.Principal, error) {
	owner := token.Spec.Owner
	key, err := oidccore.ParseIdentityKey(owner.ID)
	if err != nil || owner.Namespace != "" || owner.Provider != key.Provider || !externalUIDPattern.MatchString(owner.UID) {
		return auth.Principal{}, ErrCredentials
	}
	provider, err := v.externalProvider(key.Provider)
	if err != nil {
		return auth.Principal{}, err
	}
	payload := externalTokenEvidence{1, token.Name, string(token.UID), owner.UID, provider.descriptor.Ref, token.CreationTimestamp.Time}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > session.MaxCookieBytes {
		return auth.Principal{}, ErrBounds
	}
	p := auth.Principal{Kind: auth.KindUser, ID: owner.ID, Provider: key.Provider, AccountUID: owner.UID, AuthMethod: auth.MethodToken, TokenID: token.Name, CredentialIssuedAt: token.CreationTimestamp.Time, External: &auth.ExternalProof{ProviderUID: provider.descriptor.Ref.UID, Origin: string(provider.descriptor.Ref.Origin), Evidence: v.tokenSealer.Seal(raw)}}
	return v.loadExternal(p, now)
}
func (v *View) loadExternalToken(p auth.Principal, provider compiledProvider, now time.Time) (auth.Principal, error) {
	if p.External.SessionID != "" || len(p.External.Evidence) > session.MaxCookieBytes {
		return auth.Principal{}, ErrCredentials
	}
	raw, _, err := v.tokenSealer.Open(p.External.Evidence)
	if err != nil {
		return auth.Principal{}, ErrCredentials
	}
	var payload externalTokenEvidence
	if !strictOwner(raw, &payload, session.MaxCookieBytes) || payload.FormatVersion != 1 || payload.Provider != provider.descriptor.Ref || payload.TokenID != p.TokenID || payload.AccountUID != p.AccountUID {
		return auth.Principal{}, ErrCredentials
	}
	token, ok := v.cut.objects[store.Key{Kind: "APIToken", Name: p.TokenID}].(*internalv1.APIToken)
	if !ok || token.DeletionTimestamp != nil || string(token.UID) != payload.TokenUID || !token.CreationTimestamp.Time.Equal(payload.IssuedAt) || token.CreationTimestamp.After(now) || token.Spec.ExpiresAt != nil && !now.Before(token.Spec.ExpiresAt.Time) || token.Spec.Owner.Kind != internalv1.OwnerUser || token.Spec.Owner.UID != p.AccountUID || token.Spec.Owner.ID != p.ID || token.Spec.Owner.Provider != provider.descriptor.Ref.ID || token.Spec.Owner.Namespace != "" {
		return auth.Principal{}, ErrCredentials
	}
	var existing *ExternalIdentity
	for _, row := range v.externalIdentities {
		if row.UID == p.AccountUID {
			detached := row
			existing = &detached
			break
		}
	}
	if existing == nil || existing.Disabled || existing.Provider != provider.descriptor.Ref || existing.CreatedAt.After(payload.IssuedAt) || existing.GroupsVerifiedAt != nil && existing.GroupsVerifiedAt.After(now) {
		return auth.Principal{}, ErrCredentials
	}
	identity := oidccore.Identity{Provider: existing.Provider.ID, Issuer: existing.Issuer, Subject: existing.Subject, Username: existing.Username, Groups: slices.Clone(existing.Groups)}
	if identity.ID() != p.ID {
		return auth.Principal{}, ErrCredentials
	}
	current, err := v.externalGrants(provider, identity, existing, true)
	if err != nil {
		return auth.Principal{}, err
	}
	if err = v.externalFloors(provider, existing, token.Spec.Grants, payload.IssuedAt); err != nil {
		return auth.Principal{}, err
	}
	return detachedPrincipal(auth.Principal{Kind: auth.KindUser, ID: identity.ID(), Name: identity.Username, Groups: identity.Groups, Provider: identity.Provider, AccountUID: existing.UID, AuthMethod: auth.MethodToken, TokenID: token.Name, CredentialIssuedAt: payload.IssuedAt, GrantsIssuedAt: payload.IssuedAt, Grants: intersect(token.Spec.Grants, current), Scopes: slices.Clone(token.Spec.Scopes), External: p.External}), nil
}
