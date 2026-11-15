package security

import (
	"errors"
	"slices"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Compact well before the section cap, reserving room for genuinely live state.
// This is a work trigger, not a limit on the number of historical IAM changes.
const compactHistoryRows = 256

func (s *State) rowCount() int {
	v := s.value
	return 1 + s.otherRows + len(v.GrantFloors) + len(v.AccountFloors) + len(v.ProviderFloors) + len(v.Sessions) + len(v.TOTP) + len(v.ExternalIdentities) + len(v.ExternalSessions) + len(v.BrowserAttempts)
}

func (s *State) compactIfNeeded() error {
	if s.rowCount() >= servingauth.MaxRows || len(s.value.GrantFloors)+len(s.value.AccountFloors)+len(s.value.ProviderFloors) >= compactHistoryRows {
		return s.compact()
	}
	return nil
}

// compact materializes the effect of floors before removing their history. It
// runs inside the original admitted command, so token deletions, enrollment
// removals, the checkpoint and the IAM mutation either all commit or roll back.
// No wall-clock TTL can safely expire a revocation of an indefinitely valid JWT.
func (s *State) compact() error {
	grantFloors := make(map[string]time.Time, len(s.value.GrantFloors))
	accountFloors := make(map[servingauth.AccountRef]time.Time, len(s.value.AccountFloors))
	providerFloors := make(map[providerIncarnation]time.Time, len(s.value.ProviderFloors))
	for _, f := range s.value.GrantFloors {
		grantFloors[f.Grant] = f.ValidAfter
	}
	for _, f := range s.value.AccountFloors {
		accountFloors[f.Account] = f.ValidAfter
	}
	for _, f := range s.value.ProviderFloors {
		providerFloors[providerIncarnation{f.Provider, f.UID}] = f.ValidAfter
	}
	deniedGrant := func(grants []string, issued time.Time) bool {
		for _, g := range grants {
			if deniedAt(grantFloors[g], issued) {
				return true
			}
		}
		return false
	}
	// Materialize only affected tokens. Unaffected long-lived API tokens retain
	// their original row, UID, secret and creation time across every checkpoint.
	if len(grantFloors)+len(accountFloors)+len(providerFloors) > 0 {
		list, err := s.tx.List(s.ctx, "APIToken", store.ListOptions{Limit: servingauth.MaxObjects + 1})
		if err != nil {
			return err
		}
		if len(list.Items) > servingauth.MaxObjects || list.Continue != "" {
			return servingauth.ErrBounds
		}
		for _, obj := range list.Items {
			token, ok := obj.(*internalv1.APIToken)
			if !ok || token == nil {
				return servingauth.ErrInvalid
			}
			at := token.CreationTimestamp.Time
			a, provider := s.tokenAuthority(token.Spec.Owner)
			if deniedGrant(token.Spec.Grants, at) || deniedAt(accountFloors[a], at) || deniedAt(providerFloors[provider], at) {
				if err := s.tx.Delete(s.ctx, store.KeyOf(token), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					return err
				}
			}
		}
	}
	before := s.rowCount()
	s.value.Sessions = slices.DeleteFunc(s.value.Sessions, func(r servingauth.SessionRecord) bool {
		return r.Revoked || deniedAt(accountFloors[r.Account], r.IssuedAt) || deniedGrant(r.Grants, r.IssuedAt)
	})
	s.value.ExternalSessions = slices.DeleteFunc(s.value.ExternalSessions, func(r servingauth.ExternalSessionRecord) bool {
		return r.Revoked || deniedAt(accountFloors[r.Account], r.IssuedAt) || deniedAt(providerFloors[providerIncarnation{r.Provider.ID, r.Provider.UID}], r.IssuedAt) || deniedGrant(r.Grants, r.IssuedAt)
	})
	s.value.BrowserAttempts = slices.DeleteFunc(s.value.BrowserAttempts, func(r servingauth.BrowserAttempt) bool {
		return deniedAt(providerFloors[providerIncarnation{r.Provider.ID, r.Provider.UID}], r.IssuedAt)
	})
	// Disabled factor replay rows must survive rotation/recovery of a live UID.
	// A deleted UID can never supply a current password or confirm an old setup.
	var collectErr error
	s.value.TOTP = slices.DeleteFunc(s.value.TOTP, func(r totp.Record) bool {
		var user internalv1.LocalUser
		err := s.tx.Get(s.ctx, store.Key{Kind: "LocalUser", Name: r.Account.Name}, &user)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			collectErr = err
			return false
		}
		return errors.Is(err, store.ErrNotFound) || string(user.UID) != r.Account.UID
	})
	if collectErr != nil {
		return collectErr
	}
	advance := func(at time.Time) {
		if at.After(s.value.RevocationCheckpoint) {
			s.value.RevocationCheckpoint = at.UTC()
			s.changed = true
		}
	}
	for _, f := range s.value.GrantFloors {
		advance(f.ValidAfter)
	}
	s.value.GrantFloors = s.value.GrantFloors[:0]
	s.value.AccountFloors = slices.DeleteFunc(s.value.AccountFloors, func(f servingauth.AccountFloor) bool {
		// External floors also bind administrative revisions and prohibit silent
		// reprovisioning of missing tuples. They are live identity state.
		if f.Account.Kind == "ExternalIdentity" {
			return false
		}
		advance(f.ValidAfter)
		return true
	})
	s.value.ProviderFloors = slices.DeleteFunc(s.value.ProviderFloors, func(f servingauth.ProviderFloor) bool {
		// Enrolled external evidence has its own original token issue time.
		// Retain the exact provider floor while such evidence can still be used;
		// a newer enrollment timestamp cannot excuse older, revoked evidence.
		for _, r := range s.value.ExternalSessions {
			if r.Provider.ID == f.Provider && r.Provider.UID == f.UID {
				return false
			}
		}
		advance(f.ValidAfter)
		return true
	})
	s.changed = s.changed || before != s.rowCount()
	return nil
}

type providerIncarnation struct{ id, uid string }

func deniedAt(floor, issued time.Time) bool { return !floor.IsZero() && !issued.After(floor) }

func (s *State) tokenAuthority(o internalv1.TokenOwner) (servingauth.AccountRef, providerIncarnation) {
	a := servingauth.AccountRef{Namespace: o.Namespace, Name: o.Name, UID: o.UID}
	if o.Kind == internalv1.OwnerServiceAccount {
		a.Kind = "ServiceAccount"
	} else if o.Provider == "local" || o.Provider == "" {
		a.Kind = "LocalUser"
	} else {
		for _, i := range s.value.ExternalIdentities {
			if i.UID == o.UID && i.Provider.ID == o.Provider {
				a, _ = servingauth.ExternalAccount(i)
				return a, providerIncarnation{i.Provider.ID, i.Provider.UID}
			}
		}
	}
	return a, providerIncarnation{}
}
