package security

import (
	"context"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// AdministerExternal stages one exact-account operation in the caller's
// authorized command. Errors MUST abort that command. Enabling preserves all
// floors and never restores credentials; disabling and logout revoke exact
// sessions and personal tokens atomically with the account floor.
func AdministerExternal(ctx context.Context, tx *sqlite.CommandTx, name, uid, revision, action string, observed time.Time) error {
	if name == "" || uid == "" || revision == "" || observed.IsZero() || (action != "disable" && action != "enable" && action != "logout") {
		return servingauth.ErrInvalid
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return err
	}
	index := -1
	var account servingauth.AccountRef
	for j, identity := range s.value.ExternalIdentities {
		a, err := servingauth.ExternalAccount(identity)
		if err != nil {
			return err
		}
		if a.Name == name {
			index, account = j, a
			break
		}
	}
	if index < 0 {
		return store.ErrNotFound
	}
	i := s.value.ExternalIdentities[index]
	var floor time.Time
	for _, f := range s.value.AccountFloors {
		if f.Account == account {
			floor = f.ValidAfter
		}
	}
	want, err := servingauth.ExternalAccountRevision(i, floor)
	if err != nil {
		return err
	}
	if i.UID != uid || want != revision {
		return store.ErrConflict
	}
	if observed.Before(i.CreatedAt) || observed.Before(floor) || i.GroupsVerifiedAt != nil && observed.Before(*i.GroupsVerifiedAt) {
		return servingauth.ErrCredentials
	}
	if action == "enable" {
		if i.Disabled {
			s.value.ExternalIdentities[index].Disabled = false
			s.changed = true
		}
		return commitExternalAdmin(ctx, tx, s, account, action, observed)
	}
	if err := s.InvalidateAccount(account, observed); err != nil {
		return err
	}
	if action == "disable" && !i.Disabled {
		s.value.ExternalIdentities[index].Disabled = true
		s.changed = true
	}
	for j, session := range s.value.ExternalSessions {
		if session.Account == account && !session.Revoked {
			s.value.ExternalSessions[j].Revoked = true
			s.changed = true
		}
	}
	list, err := tx.List(ctx, "APIToken", store.ListOptions{Limit: servingauth.MaxObjects + 1})
	if err != nil {
		return err
	}
	if len(list.Items) > servingauth.MaxObjects || list.Continue != "" {
		return servingauth.ErrBounds
	}
	id := (oidccore.Identity{Provider: i.Provider.ID, Issuer: i.Issuer, Subject: i.Subject}).ID()
	for _, object := range list.Items {
		token, ok := object.(*internalv1.APIToken)
		if !ok {
			return servingauth.ErrInvalid
		}
		o := token.Spec.Owner
		if o.Kind != internalv1.OwnerUser || o.Namespace != "" || o.UID != i.UID || o.Provider != i.Provider.ID || o.ID != id {
			continue
		}
		// The immutable UID/tuple, not a mutable username, owns these tokens.
		if err := tx.Delete(ctx, store.KeyOf(token), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
	}
	return commitExternalAdmin(ctx, tx, s, account, action, observed)
}

func commitExternalAdmin(ctx context.Context, tx *sqlite.CommandTx, s *State, account servingauth.AccountRef, action string, observed time.Time) error {
	if err := s.Commit(ctx); err != nil {
		return err
	}
	return tx.RecordAudit(ctx, sqlite.AuditEntry{Time: observed, Manager: string(store.ManagerAPI), Action: "externalidentity." + action, Key: store.Key{Kind: "ExternalIdentity", Name: account.Name}, Reason: "exact account administration"})
}
