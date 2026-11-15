package tokens

import (
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func accountOwner(sa *internalv1.ServiceAccount) internalv1.TokenOwner {
	return internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: sa.Name, Namespace: sa.Namespace,
		UID: string(sa.UID), ID: ServiceAccountID(sa.Namespace, sa.Name), Provider: "kudurru"}
}

// BuiltinAdmin reads private authority for this exact account incarnation.
// Ordinary object fields and annotations are never consulted for privileges.
func (t *Tokens) BuiltinAdmin(sa *internalv1.ServiceAccount) bool {
	if sa == nil || sa.UID == "" || sa.Spec.Disabled {
		return false
	}
	owner := accountOwner(sa)
	state, ok := t.principals.Get(store.Key{Kind: internalv1.KindPrincipalState, Name: internalv1.PrincipalStateName(owner)})
	return ok && state.Spec.Owner == owner && state.Spec.BuiltinAdmin
}
