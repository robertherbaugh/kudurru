package security

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Apply stages the ordinary object mutation and any resulting revocation in one
// admitted command. The caller MUST propagate its error to Command. This is not
// a replacement for ReauthorizeCurrent or the object's public write restrictions.
func Apply(ctx context.Context, tx *sqlite.CommandTx, obj store.Object, opts store.ApplyOptions, now time.Time) (store.Object, error) {
	if obj == nil || reflect.ValueOf(obj).IsNil() || now.IsZero() {
		return nil, servingauth.ErrInvalid
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	before := obj.DeepCopyObject().(store.Object)
	if err := tx.Get(ctx, store.KeyOf(obj), before); errors.Is(err, store.ErrNotFound) {
		before = nil
	} else if err != nil {
		return nil, err
	}
	out, err := tx.Apply(ctx, obj, opts)
	if err != nil {
		return nil, err
	}
	if err := s.objectChange(before, out, now); err != nil {
		return nil, err
	}
	if err := s.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete takes a typed object only as a key/template. Current identity comes
// from this transaction, never from the caller's possibly stale object. Setting
// deletionTimestamp under finalizers already invalidates its old sessions.
func Delete(ctx context.Context, tx *sqlite.CommandTx, template store.Object, opts store.DeleteOptions, now time.Time) error {
	if template == nil || reflect.ValueOf(template).IsNil() || now.IsZero() {
		return servingauth.ErrInvalid
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return err
	}
	before := template.DeepCopyObject().(store.Object)
	if err := tx.Get(ctx, store.KeyOf(template), before); err != nil {
		return err
	}
	if err := tx.Delete(ctx, store.KeyOf(before), opts); err != nil {
		return err
	}
	if err := s.objectChange(before, nil, now); err != nil {
		return err
	}
	return s.Commit(ctx)
}

func accountRef(obj store.Object) servingauth.AccountRef {
	return servingauth.AccountRef{Kind: string(store.KindOf(obj)), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}

func (s *State) objectChange(before, after store.Object, now time.Time) error {
	if before == nil {
		return nil
	}
	if after != nil && (after.GetUID() != before.GetUID() || after.GetDeletionTimestamp() != nil) {
		after = nil
	}
	switch old := before.(type) {
	case *publicv1.RoleBinding:
		prior := rbac.FromRoleBinding(old)
		grant := rbac.Grant{Role: prior.Role, Scope: prior.Scope}.Normalized()
		if next, ok := after.(*publicv1.RoleBinding); ok {
			binding := rbac.FromRoleBinding(next)
			if (rbac.Grant{Role: binding.Role, Scope: binding.Scope}).Normalized() == grant {
				retained := true
				for _, subject := range prior.Subjects {
					retained = retained && slices.Contains(binding.Subjects, subject)
				}
				if retained {
					return nil
				}
			}
		}
		return s.InvalidateGrant(grant.String(), now)
	case *internalv1.LocalUser:
		if next, ok := after.(*internalv1.LocalUser); ok {
			a, b := old.Spec, next.Spec
			a.DisplayName, b.DisplayName = "", ""
			if reflect.DeepEqual(a, b) {
				return nil
			}
		}
		return s.InvalidateAccount(accountRef(old), now)
	case *publicv1.ServiceAccount:
		if next, ok := after.(*publicv1.ServiceAccount); ok && old.Spec.Disabled == next.Spec.Disabled {
			return nil
		}
		return s.InvalidateAccount(accountRef(old), now)
	case *publicv1.IdentityProvider:
		if next, ok := after.(*publicv1.IdentityProvider); ok && reflect.DeepEqual(old.Spec, next.Spec) {
			return nil
		}
		return s.InvalidateProvider(old.Name, string(old.UID), now)
	case *internalv1.PrincipalState:
		if next, ok := after.(*internalv1.PrincipalState); ok && reflect.DeepEqual(old.Spec, next.Spec) {
			return nil
		}
		owner := old.Spec.Owner
		if owner.Kind == internalv1.OwnerServiceAccount {
			return s.InvalidateAccount(servingauth.AccountRef{Kind: "ServiceAccount", Namespace: owner.Namespace, Name: owner.Name, UID: owner.UID}, now)
		}
	}
	return nil
}
