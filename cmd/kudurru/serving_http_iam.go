package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func (h *servingHTTP) iamUsers(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "users", "read")
	if err != nil {
		return err
	}
	objects, err := v.Objects("LocalUser")
	if err != nil {
		return err
	}
	out := []api.IAMUser{}
	for _, o := range objects {
		u := o.(*internalv1.LocalUser)
		p := api.IAMUser{Name: u.Name, UID: string(u.UID), ResourceVersion: u.ResourceVersion, DisplayName: u.Spec.DisplayName, BuiltinAdmin: u.Spec.BuiltinAdmin, Disabled: u.Spec.Disabled, MustChangePassword: u.Spec.MustChangePassword, Groups: slices.Clone(u.Spec.Groups)}
		if u.Status.LastLoginAt != nil {
			at := u.Status.LastLoginAt.Time
			p.LastLoginAt = &at
		}
		if u.Status.PasswordChangedAt != nil {
			at := u.Status.PasswordChangedAt.Time
			p.PasswordChangedAt = &at
		}
		out = append(out, p)
	}
	return servingHTTPList(w, r, out)
}
func (h *servingHTTP) iamServiceAccounts(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "serviceaccounts", "read")
	if err != nil {
		return err
	}
	sas, err := v.Objects("ServiceAccount")
	if err != nil {
		return err
	}
	tokens, err := v.Objects("APIToken")
	if err != nil {
		return err
	}
	states, err := v.Objects("PrincipalState")
	if err != nil {
		return err
	}
	out := []api.ServiceAccountInfo{}
	for _, o := range sas {
		sa := o.(*publicv1.ServiceAccount)
		p := api.ServiceAccountInfo{Namespace: sa.Namespace, Name: sa.Name, Description: sa.Spec.Description, Disabled: sa.Spec.Disabled}
		owner := internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: sa.Name, Namespace: sa.Namespace, UID: string(sa.UID), ID: "sa:" + sa.Namespace + "/" + sa.Name, Provider: "kudurru"}
		for _, o := range states {
			s := o.(*internalv1.PrincipalState)
			if s.DeletionTimestamp == nil && s.Spec.Owner == owner && s.Spec.BuiltinAdmin {
				p.BuiltinAdmin = true
			}
		}
		for _, o := range tokens {
			t := o.(*internalv1.APIToken)
			if t.Spec.Owner.Kind == owner.Kind && t.Spec.Owner.Name == owner.Name && t.Spec.Owner.Namespace == owner.Namespace && t.Spec.Owner.UID == owner.UID {
				p.Tokens++
			}
		}
		out = append(out, p)
	}
	return servingHTTPList(w, r, out)
}
func (h *servingHTTP) iamRoleBindings(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "rolebindings", "read")
	if err != nil {
		return err
	}
	objects, err := v.Objects("RoleBinding")
	if err != nil {
		return err
	}
	out := []api.RoleBindingInfo{}
	add := func(b rbac.Binding, source, namespace, name, manager string) {
		subjects := []api.BindingSubject{}
		for _, s := range b.Subjects {
			subjects = append(subjects, api.BindingSubject{Kind: string(s.Kind), Name: s.Name, Namespace: s.Namespace})
		}
		scope := (rbac.Grant{Role: b.Role, Scope: b.Scope}).Normalized().Scope
		out = append(out, api.RoleBindingInfo{Source: source, Namespace: namespace, Name: name, Role: b.Role, Scope: api.BindingScope{Kind: string(scope.Kind), Name: scope.Name}, Subjects: subjects, ManagedBy: manager})
	}
	bindings, err := rbac.ParseBindings(h.opts.Runtime.opts.Config.RBAC.Bindings)
	if err != nil {
		return err
	}
	for i, b := range bindings {
		add(b, "config", "", fmt.Sprintf("rbac.bindings[%d]", i), "")
	}
	for _, o := range objects {
		b := o.(*publicv1.RoleBinding)
		add(rbac.FromRoleBinding(b), "object", b.Namespace, b.Name, store.ManagedBy(b))
	}
	return servingHTTPList(w, r, out)
}
func (h *servingHTTP) providerInfos(v *servingauth.View) []api.IdentityProviderInfo {
	ids := map[string]bool{}
	objects, err := v.Objects("IdentityProvider")
	if err != nil {
		return nil
	}
	for _, o := range objects {
		ids[o.GetName()] = true
	}
	for _, m := range h.opts.Runtime.opts.Config.Auth.IdentityProviders {
		if id, ok := m["id"].(string); ok {
			ids[id] = true
		}
	}
	out := []api.IdentityProviderInfo{}
	for id := range ids {
		info, err := v.ProviderInfo(id)
		if err != nil {
			continue
		}
		d := info.Descriptor
		s := d.Spec
		out = append(out, api.IdentityProviderInfo{ID: id, DisplayName: s.DisplayName, Issuer: d.Issuer, ClientID: s.ClientID, CLIClientID: s.CLIClientID, Scopes: append([]string{}, s.Scopes...), RedirectURL: s.RedirectURL, Source: string(d.Ref.Origin)})
	}
	slices.SortFunc(out, func(a, b api.IdentityProviderInfo) int { return strings.Compare(a.ID, b.ID) })
	return out
}
func (h *servingHTTP) iamProviders(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "identityproviders", "read")
	if err != nil {
		return err
	}
	return servingHTTPList(w, r, h.providerInfos(v))
}
