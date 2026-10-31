package servingauth

import (
	"context"
	"slices"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ExternalLoginProof is raw current login proof, never a forwarded Principal.
type ExternalLoginProof struct {
	Provider                       ProviderRef
	SpecIdentity, ResourceIdentity string
	RawIDToken                     string
	Mode                           oidccore.ProofMode
	Browser                        oidccore.BrowserProof
}

func (ExternalLoginProof) String() string { return "private external login proof" }

// GoString prevents raw login proof disclosure.
func (p ExternalLoginProof) GoString() string { return p.String() }

// MarshalJSON prevents accidental credential persistence.
func (ExternalLoginProof) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// ExternalSessionDecision is detached; private verified source cannot be forged.
type ExternalSessionDecision struct {
	Identity    oidccore.Identity
	Provider    ProviderDescriptor
	Existing    *ExternalIdentity
	Grants      []string
	Settings    *publicv1.Kudurru
	evidence    *oidccore.VerifiedToken
	mode        oidccore.ProofMode
	verifiedAt  time.Time
	provider    ProviderRef
	existingUID string
	source      oidccore.EvidenceSource
}

func (ExternalSessionDecision) String() string { return "private external session decision" }

// GoString prevents verified claim disclosure.
func (d ExternalSessionDecision) GoString() string { return d.String() }

// MarshalJSON prevents accidental verified claim persistence.
func (ExternalSessionDecision) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// ValidAt rechecks original proof expiry during the same admitted issuance command.
func (d ExternalSessionDecision) ValidAt(now time.Time) error {
	if d.evidence == nil || now.Before(d.verifiedAt) {
		return ErrCredentials
	}
	return d.evidence.ValidAt(now)
}

// VerifiedProofExpiresAt returns the original verified proof deadline. It is
// provenance for a logout hint, never the lifetime of an enrolled session.
func (d ExternalSessionDecision) VerifiedProofExpiresAt() time.Time {
	if d.evidence == nil {
		return time.Time{}
	}
	return d.evidence.Facts().ExpiresAt
}

// SealEvidence requires the exact existing account UID or a new independently generated UID.
func (d ExternalSessionDecision) SealEvidence(k *oidccore.EvidenceKeyring, identityUID, sessionID string) (string, error) {
	if d.evidence == nil || !externalUIDPattern.MatchString(identityUID) || d.existingUID != "" && identityUID != d.existingUID {
		return "", ErrCredentials
	}
	f := d.evidence.Facts()
	b := oidccore.EvidenceBinding{Kind: oidccore.SessionEvidence, Provider: d.provider.ID, ProviderUID: d.provider.UID, Origin: string(d.provider.Origin), Issuer: f.Issuer, Subject: f.Subject, IdentityUID: identityUID, RecordID: sessionID}
	return k.SealVerified(b, d.source, d.evidence, d.mode, d.verifiedAt)
}

// ResolveExternalSessionCurrent verifies raw login and current grants/settings in one concrete transaction.
func ResolveExternalSessionCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof ExternalLoginProof, now time.Time) (ExternalSessionDecision, error) {
	v, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return ExternalSessionDecision{}, err
	}
	defer v.Release()
	provider, err := v.externalProvider(proof.Provider.ID)
	if err != nil || provider.descriptor.Ref != proof.Provider || provider.descriptor.SpecIdentity != proof.SpecIdentity || provider.identity != proof.ResourceIdentity {
		return ExternalSessionDecision{}, ErrDependency
	}
	var token *oidccore.VerifiedToken
	switch proof.Mode {
	case oidccore.BrowserIDToken:
		token, err = provider.resource.Verifier.VerifyBrowser(ctx, proof.RawIDToken, proof.Browser, now)
	case oidccore.BearerIDToken:
		if proof.Browser.Nonce != "" || proof.Browser.AccessToken != "" {
			return ExternalSessionDecision{}, ErrCredentials
		}
		token, err = provider.resource.Verifier.Verify(ctx, proof.RawIDToken, now)
	default:
		return ExternalSessionDecision{}, ErrCredentials
	}
	if err != nil {
		return ExternalSessionDecision{}, ErrCredentials
	}
	identity, err := provider.policy.Evaluate(ctx, token, proof.Mode, now)
	if err != nil {
		return ExternalSessionDecision{}, ErrCredentials
	}
	existing, err := v.externalAccount(provider, identity, now)
	if err != nil {
		return ExternalSessionDecision{}, err
	}
	grants, err := v.externalGrants(provider, identity, existing, false)
	if err != nil {
		return ExternalSessionDecision{}, err
	}
	if len(grants) == 0 {
		return ExternalSessionDecision{}, ErrDenied
	}
	if err = v.externalFloors(provider, existing, grants, token.Facts().IssuedAt); err != nil {
		return ExternalSessionDecision{}, err
	}
	settings, err := v.Settings()
	if err != nil {
		return ExternalSessionDecision{}, err
	}
	info, err := v.ProviderInfo(provider.descriptor.Ref.ID)
	if err != nil {
		return ExternalSessionDecision{}, err
	}
	d := ExternalSessionDecision{Identity: identity, Provider: info.Descriptor, Existing: existing, Grants: grants, Settings: settings, evidence: token, mode: proof.Mode, verifiedAt: now, provider: provider.descriptor.Ref, source: provider.evidenceSource()}
	if existing != nil {
		d.existingUID = existing.UID
	}
	return d, nil
}
func (p compiledProvider) evidenceSource() oidccore.EvidenceSource {
	return oidccore.EvidenceSource{SpecIdentity: p.descriptor.SpecIdentity, PolicyIdentity: p.descriptor.PolicyIdentity, ResourceIdentity: p.identity}
}
func (v *View) externalProvider(id string) (compiledProvider, error) {
	if v == nil || v.released.Load() || !v.license.Providers {
		return compiledProvider{}, ErrCredentials
	}
	p, ok := v.providerViews[id]
	if !ok || p.resource.Availability != ProviderReady {
		return compiledProvider{}, ErrCredentials
	}
	return p, nil
}
func (v *View) externalAccount(provider compiledProvider, identity oidccore.Identity, now time.Time) (*ExternalIdentity, error) {
	name, err := ExternalIdentityName(provider.descriptor.Ref, identity.Issuer, identity.Subject)
	if err != nil {
		return nil, ErrCredentials
	}
	for account, i := range v.externalIdentities {
		if account.Name != name {
			continue
		}
		if i.Disabled || i.CreatedAt.After(now) || i.GroupsVerifiedAt != nil && i.GroupsVerifiedAt.After(now) {
			return nil, ErrCredentials
		}
		detached := detachedExternal(i)
		return &detached, nil
	}
	// A retained floor for a missing tuple is revocation history, not permission
	// to silently recreate the account with an unrelated UID.
	for account := range v.accounts {
		if account.Kind == "ExternalIdentity" && account.Name == name {
			return nil, ErrCredentials
		}
	}
	if provider.descriptor.Spec.Provisioning.AutoProvision == "never" {
		return nil, ErrCredentials
	}
	return nil, nil
}
func (v *View) externalGrants(provider compiledProvider, identity oidccore.Identity, existing *ExternalIdentity, enforceSync bool) ([]string, error) {
	if !v.license.RBAC {
		return []string{}, nil
	}
	if enforceSync && provider.descriptor.Spec.Provisioning.SyncGroups && existing != nil && existing.GroupsVerifiedAt != nil {
		identity.Groups = intersect(identity.Groups, existing.Groups)
	}
	principal := auth.Principal{Kind: auth.KindUser, ID: identity.ID(), Name: identity.Username, Provider: identity.Provider, Groups: identity.Groups}
	result := []string{}
	for _, grant := range rbac.GrantsFor(v.bindings, auth.RBACPrincipal(principal)) {
		result = append(result, grant.Normalized().String())
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) == 0 && provider.descriptor.Spec.Provisioning.AutoProvision != "mapped-only" && provider.descriptor.Spec.Provisioning.DefaultRole != "" {
		result = append(result, provider.descriptor.Spec.Provisioning.DefaultRole)
	}
	if len(result) > MaxGrants {
		return nil, ErrBounds
	}
	return result, nil
}
func intersect(a, b []string) []string {
	result := []string{}
	for _, s := range a {
		if slices.Contains(b, s) && !slices.Contains(result, s) {
			result = append(result, s)
		}
	}
	slices.Sort(result)
	return result
}
func (v *View) externalFloors(provider compiledProvider, existing *ExternalIdentity, grants []string, issuedAt time.Time) error {
	if issuedAt.Before(provider.descriptor.CreatedAt) {
		return ErrCredentials
	}
	if floor := v.providers[providerKey(provider.descriptor.Ref.ID, provider.descriptor.Ref.UID)]; !floor.IsZero() && !issuedAt.After(floor) {
		return ErrCredentials
	}
	if existing != nil {
		account, _ := ExternalAccount(*existing)
		if floor := v.accounts[account]; !floor.IsZero() && !issuedAt.After(floor) {
			return ErrCredentials
		}
	}
	for _, grant := range grants {
		if !v.knownGrant(grant) {
			return ErrCredentials
		}
		if floor := v.floors[grant]; !floor.IsZero() && !issuedAt.After(floor) {
			return ErrCredentials
		}
	}
	return nil
}
func requestBinding(provider compiledProvider, identity oidccore.Identity, uid string) oidccore.EvidenceBinding {
	return oidccore.EvidenceBinding{Kind: oidccore.RequestEvidence, Provider: provider.descriptor.Ref.ID, ProviderUID: provider.descriptor.Ref.UID, Origin: string(provider.descriptor.Ref.Origin), Issuer: identity.Issuer, Subject: identity.Subject, IdentityUID: uid}
}
func (v *View) verifyJWT(ctx context.Context, proof Credential, now time.Time) (auth.Principal, error) {
	p, _, err := v.verifyJWTWithValidity(ctx, proof, now)
	return p, err
}
func (v *View) verifyJWTWithValidity(ctx context.Context, proof Credential, now time.Time) (auth.Principal, credentialValidity, error) {
	issuer, err := oidccore.RoutingIssuer(proof.Value)
	if err != nil {
		return auth.Principal{}, credentialValidity{}, ErrCredentials
	}
	id := proof.ProviderID
	if id == "" {
		for candidate, p := range v.providerViews {
			if p.descriptor.Issuer == issuer {
				id = candidate
				break
			}
		}
	}
	provider, err := v.externalProvider(id)
	if err != nil || provider.descriptor.Issuer != issuer {
		return auth.Principal{}, credentialValidity{}, ErrCredentials
	}
	token, err := provider.resource.Verifier.Verify(ctx, proof.Value, now)
	if err != nil {
		return auth.Principal{}, credentialValidity{}, ErrCredentials
	}
	identity, err := provider.policy.Evaluate(ctx, token, oidccore.BearerIDToken, now)
	if err != nil {
		return auth.Principal{}, credentialValidity{}, ErrCredentials
	}
	existing, err := v.externalAccount(provider, identity, now)
	if err != nil {
		return auth.Principal{}, credentialValidity{}, err
	}
	if provider.descriptor.Spec.Provisioning.SyncGroups && existing != nil && existing.GroupsVerifiedAt != nil {
		identity.Groups = intersect(identity.Groups, existing.Groups)
	}
	grants, err := v.externalGrants(provider, identity, existing, true)
	if err != nil || len(grants) == 0 {
		return auth.Principal{}, credentialValidity{}, ErrDenied
	}
	if err = v.externalFloors(provider, existing, grants, token.Facts().IssuedAt); err != nil {
		return auth.Principal{}, credentialValidity{}, err
	}
	uid := ""
	if existing != nil {
		uid = existing.UID
	}
	evidence, err := v.evidenceKeys.SealVerified(requestBinding(provider, identity, uid), provider.evidenceSource(), token, oidccore.BearerIDToken, now)
	if err != nil {
		return auth.Principal{}, credentialValidity{}, ErrCredentials
	}
	return detachedPrincipal(auth.Principal{Kind: auth.KindUser, ID: identity.ID(), Name: identity.Username, Provider: identity.Provider, AccountUID: uid, AuthMethod: auth.MethodJWT, Groups: identity.Groups, Grants: grants, GrantsIssuedAt: token.Facts().IssuedAt, CredentialIssuedAt: token.Facts().IssuedAt, External: &auth.ExternalProof{ProviderUID: provider.descriptor.Ref.UID, Origin: string(provider.descriptor.Ref.Origin), Evidence: evidence}}), credentialValidity{kind: OIDCJWT, authenticatedAt: now, jwt: token}, nil
}
func (v *View) loadExternal(p auth.Principal, now time.Time) (auth.Principal, error) {
	if p.Kind != auth.KindUser || p.External == nil || now.IsZero() {
		return auth.Principal{}, ErrCredentials
	}
	key, err := oidccore.ParseIdentityKey(p.ID)
	if err != nil || key.Provider != p.Provider {
		return auth.Principal{}, ErrCredentials
	}
	provider, err := v.externalProvider(key.Provider)
	if err != nil || p.External.ProviderUID != provider.descriptor.Ref.UID || p.External.Origin != string(provider.descriptor.Ref.Origin) || key.Issuer != provider.descriptor.Issuer {
		return auth.Principal{}, ErrCredentials
	}
	if p.AuthMethod == auth.MethodCookie {
		record, ok := v.externalSessions[p.External.SessionID]
		if !ok || p.External.Evidence != "" || p.AccountUID != record.Account.UID || key.Issuer != record.Issuer || key.Subject != record.Subject {
			return auth.Principal{}, ErrCredentials
		}
		return v.externalSessionPrincipal(context.Background(), record, now)
	}
	if p.AuthMethod == auth.MethodToken {
		return v.loadExternalToken(p, provider, now)
	}
	if p.AuthMethod != auth.MethodJWT || p.External.SessionID != "" {
		return auth.Principal{}, ErrCredentials
	}
	identity := oidccore.Identity{Provider: key.Provider, Issuer: key.Issuer, Subject: key.Subject}
	evidence, err := v.evidenceKeys.Open(requestBinding(provider, identity, p.AccountUID), p.External.Evidence)
	if err != nil || evidence.Source() != provider.evidenceSource() {
		return auth.Principal{}, ErrCredentials
	}
	identity, err = provider.policy.EvaluateRequest(context.Background(), evidence, now)
	if err != nil {
		return auth.Principal{}, ErrCredentials
	}
	existing, err := v.externalAccount(provider, identity, now)
	if err != nil {
		return auth.Principal{}, err
	}
	uid := ""
	if existing != nil {
		uid = existing.UID
	}
	if uid != p.AccountUID {
		return auth.Principal{}, ErrCredentials
	}
	if provider.descriptor.Spec.Provisioning.SyncGroups && existing != nil && existing.GroupsVerifiedAt != nil {
		identity.Groups = intersect(identity.Groups, existing.Groups)
	}
	grants, err := v.externalGrants(provider, identity, existing, true)
	if err != nil {
		return auth.Principal{}, err
	}
	if err = v.externalFloors(provider, existing, grants, evidence.Facts().IssuedAt); err != nil {
		return auth.Principal{}, err
	}
	return detachedPrincipal(auth.Principal{Kind: auth.KindUser, ID: identity.ID(), Name: identity.Username, Provider: identity.Provider, AccountUID: uid, AuthMethod: auth.MethodJWT, Groups: identity.Groups, Grants: grants, GrantsIssuedAt: evidence.Facts().IssuedAt, CredentialIssuedAt: evidence.Facts().IssuedAt, External: p.External}), nil
}
