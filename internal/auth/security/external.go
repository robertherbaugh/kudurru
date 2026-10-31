package security

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// IssueExternal verifies the original external proof against current command
// state, then stages identity/profile synchronization, revocation and enrollment
// atomically. Clock is the trusted host clock (nil selects time.Now). Group
// removal raises the exact account floor at observation; issuance needs a real
// later clock value. Time is never fabricated to get past a revocation floor.
//
// The outer command must consume browser preauth state and pin dependencies
// through commit. Every error must abort it. Only Enrollment.Seal against a
// complete postcommit view may produce a cookie response.
func IssueExternal(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.ExternalLoginProof, clock func() time.Time) (Enrollment, error) {
	if clock == nil {
		clock = time.Now
	}
	observedAt := clock().UTC()
	d, err := servingauth.ResolveExternalSessionCurrent(ctx, tx, deps, proof, observedAt)
	if err != nil {
		return Enrollment{}, err
	}
	cfg, err := sessionConfig(d.Settings)
	if err != nil {
		return Enrollment{}, err
	}
	lifetime, idle := cfg.Auth.Session.Lifetime.Std(), cfg.Auth.Session.IdleTimeout.Std()
	if lifetime <= 0 || idle <= 0 || idle > lifetime {
		return Enrollment{}, servingauth.ErrInvalid
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return Enrollment{}, err
	}
	if err := s.CollectExpired(observedAt); err != nil {
		return Enrollment{}, err
	}
	identity, err := s.syncExternal(d, observedAt)
	if err != nil {
		return Enrollment{}, err
	}
	issuedAt := clock().UTC()
	if err := d.ValidAt(issuedAt); err != nil {
		return Enrollment{}, servingauth.ErrCredentials
	}
	account, err := servingauth.ExternalAccount(identity)
	if err != nil {
		return Enrollment{}, err
	}
	id, csrf := randomID(), randomID()
	keys, err := oidccore.NewEvidenceKeyring(deps.CurrentKey, deps.PreviousKey)
	if err != nil {
		return Enrollment{}, err
	}
	evidence, err := d.SealEvidence(keys, identity.UID, id)
	if err != nil {
		return Enrollment{}, err
	}
	record := servingauth.ExternalSessionRecord{
		ID: id, Account: account, Provider: identity.Provider, Issuer: identity.Issuer, Subject: identity.Subject,
		IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(lifetime), IdleExpiresAt: issuedAt.Add(idle),
		Grants: slices.Clone(d.Grants), CSRFHash: servingauth.CSRFHash(csrf), Evidence: evidence,
	}
	if err := tx.RequireCommitCheck(func(context.Context) error {
		now := clock().UTC()
		if now.Before(issuedAt) || !now.Before(record.ExpiresAt) || !now.Before(record.IdleExpiresAt) || d.ValidAt(now) != nil {
			return servingauth.ErrCredentials
		}
		return nil
	}); err != nil {
		return Enrollment{}, err
	}
	if err := s.enrollExternal(record); err != nil {
		return Enrollment{}, err
	}
	if err := s.Commit(ctx); err != nil {
		return Enrollment{}, err
	}
	enrollment := Enrollment{id: id, csrf: csrf}
	if proof.Mode == oidccore.BrowserIDToken {
		expiry := d.VerifiedProofExpiresAt()
		for _, deadline := range []time.Time{record.ExpiresAt, record.IdleExpiresAt} {
			if deadline.Before(expiry) {
				expiry = deadline
			}
		}
		// An oversized hint disables only remote logout, never a valid enrollment.
		enrollment.logoutHint, _ = browseroidc.NewLogoutHint(browseroidc.LogoutBinding{Provider: proof.Provider.ID, ProviderUID: proof.Provider.UID, Origin: string(proof.Provider.Origin), SpecIdentity: proof.SpecIdentity, ResourceIdentity: proof.ResourceIdentity, Issuer: d.Provider.Issuer, ClientID: d.Provider.Spec.ClientID, RedirectURI: d.Provider.Spec.RedirectURL}, id, proof.RawIDToken, issuedAt, expiry)
	}
	return enrollment, nil
}

func (s *State) syncExternal(d servingauth.ExternalSessionDecision, now time.Time) (servingauth.ExternalIdentity, error) {
	identity := servingauth.ExternalIdentity{FormatVersion: servingauth.FormatVersion, Provider: d.Provider.Ref, Issuer: d.Identity.Issuer, Subject: d.Identity.Subject, CreatedAt: now, Groups: []string{}}
	key, err := servingauth.ExternalIdentityKey(identity.Provider, identity.Issuer, identity.Subject)
	if err != nil {
		return servingauth.ExternalIdentity{}, err
	}
	index := -1
	for i, old := range s.value.ExternalIdentities {
		oldKey, err := servingauth.ExternalIdentityKey(old.Provider, old.Issuer, old.Subject)
		if err != nil {
			return servingauth.ExternalIdentity{}, err
		}
		if oldKey == key {
			if d.Existing == nil || !reflect.DeepEqual(old, *d.Existing) {
				return servingauth.ExternalIdentity{}, ErrConflict
			}
			identity, index = old, i
			break
		}
	}
	if index < 0 {
		if d.Existing != nil {
			return servingauth.ExternalIdentity{}, ErrConflict
		}
		if err := s.capacity(); err != nil {
			return servingauth.ExternalIdentity{}, err
		}
		var uid [16]byte
		_, _ = rand.Read(uid[:])
		identity.UID = hex.EncodeToString(uid[:])
	}
	if index >= 0 && identity.Username != d.Identity.Username {
		account, err := servingauth.ExternalAccount(identity)
		if err != nil {
			return servingauth.ExternalIdentity{}, err
		}
		if err := s.InvalidateAccount(account, now); err != nil {
			return servingauth.ExternalIdentity{}, err
		}
	}
	identity.Username, identity.DisplayName, identity.Email = d.Identity.Username, d.Identity.DisplayName, d.Identity.Email
	// Initial provisioning retains the verified group snapshot. Disabling
	// ongoing sync preserves it; it never creates an invented token authority.
	if index < 0 || d.Provider.Spec.Provisioning.SyncGroups {
		for _, group := range identity.Groups {
			if !slices.Contains(d.Identity.Groups, group) {
				account, err := servingauth.ExternalAccount(identity)
				if err != nil {
					return servingauth.ExternalIdentity{}, err
				}
				if err := s.InvalidateAccount(account, now); err != nil {
					return servingauth.ExternalIdentity{}, err
				}
				break
			}
		}
		identity.Groups = append([]string{}, d.Identity.Groups...)
		identity.GroupsVerifiedAt = &now
	}
	if index < 0 {
		s.value.ExternalIdentities = append(s.value.ExternalIdentities, identity)
	} else {
		s.value.ExternalIdentities[index] = identity
	}
	s.changed = true
	return identity, nil
}

func (s *State) enrollExternal(record servingauth.ExternalSessionRecord) error {
	if err := s.valid(); err != nil {
		return err
	}
	if s.value.Epoch == math.MaxUint64 || record.Revoked {
		return servingauth.ErrInvalid
	}
	if err := s.capacity(); err != nil {
		return err
	}
	for _, old := range s.value.Sessions {
		if old.ID == record.ID {
			return ErrConflict
		}
	}
	for _, old := range s.value.ExternalSessions {
		if old.ID == record.ID {
			return ErrConflict
		}
	}
	for _, f := range s.value.AccountFloors {
		if f.Account == record.Account && !record.IssuedAt.After(f.ValidAfter) {
			return servingauth.ErrCredentials
		}
	}
	for _, f := range s.value.ProviderFloors {
		if f.Provider == record.Provider.ID && f.UID == record.Provider.UID && !record.IssuedAt.After(f.ValidAfter) {
			return servingauth.ErrCredentials
		}
	}
	for _, f := range s.value.GrantFloors {
		if slices.Contains(record.Grants, f.Grant) && !record.IssuedAt.After(f.ValidAfter) {
			return servingauth.ErrCredentials
		}
	}
	record.FormatVersion, record.Epoch = servingauth.FormatVersion, s.value.Epoch+1
	record.Grants = slices.Clone(record.Grants)
	s.value.ExternalSessions = append(s.value.ExternalSessions, record)
	s.changed = true
	return nil
}
