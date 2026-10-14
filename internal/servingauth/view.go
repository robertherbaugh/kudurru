package servingauth

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// View owns only immutable canonical/dependency policy and pure crypto. Root
// captures it through the whole publisher lease, never an independent pointer.
type View struct {
	cut                *Cut
	identity           string
	license            LicensePolicy
	permissions        *rbac.Permissions
	bindings           []rbac.Binding
	grants             map[string]bool
	floors             map[string]time.Time
	accounts           map[AccountRef]time.Time
	providers          map[string]time.Time
	sessions           map[string]SessionRecord
	sealer             *session.Sealer
	hasher             PasswordVerifier
	factors            map[totp.Account]*totp.Seed
	providerViews      map[string]compiledProvider
	providerIdentity   string
	evidenceKeys       *oidccore.EvidenceKeyring
	externalIdentities map[AccountRef]ExternalIdentity
	externalSessions   map[string]ExternalSessionRecord
	tokenSealer        *session.Sealer
	released           atomic.Bool
}

// String redacts private owner data in diagnostics.
func (*View) String() string { return "servingauth view (private)" }

// GoString redacts private owner data in Go diagnostics.
func (v *View) GoString() string { return v.String() }

// MarshalJSON prevents private projections from entering diagnostics.
func (*View) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// Base returns the complete candidate identity.
func (v *View) Base() replication.Generation { return v.cut.base }

// DependencyIdentity pins exact configured policy/key identities to publication.
func (v *View) DependencyIdentity() string { return v.identity }

// Settings returns detached canonical settings from this cut. Root combines
// these with pinned host settings; this never consults a live config reader.
func (v *View) Settings() (*publicv1.Kudurru, error) {
	if v == nil || v.released.Load() {
		return nil, ErrInvalid
	}
	return v.cut.objects[store.Key{Kind: "Kudurru", Name: "default"}].(*publicv1.Kudurru).DeepCopy(), nil
}

// Release retires this owner reference. Root must release only after last lease.
func (v *View) Release() {
	if v != nil {
		v.released.Store(true)
	}
}

// Prepare clones every policy input. Dependencies explicitly pin entitlement;
// missing/mismatched identity never falls back to a current license/config read.
func Prepare(ctx context.Context, cut *Cut, deps Dependencies) (*View, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cut == nil || deps.Identity == "" || deps.Identity != deps.ExpectedIdentity {
		return nil, ErrDependency
	}
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(deps.Permissions)
	if err != nil || len(raw) > MaxRootBytes {
		return nil, ErrBounds
	}
	var permissions rbac.Permissions
	if !strict(raw, &permissions, MaxRootBytes) {
		return nil, ErrInvalid
	}
	sealer, err := session.NewSealer(session.PurposeSession, deps.CurrentKey, deps.PreviousKey)
	if err != nil {
		return nil, ErrDependency
	}
	v := &View{cut: cut, license: deps.License, permissions: &permissions, grants: map[string]bool{}, floors: map[string]time.Time{}, accounts: map[AccountRef]time.Time{}, providers: map[string]time.Time{}, sessions: map[string]SessionRecord{}, sealer: sealer, hasher: deps.Hasher}
	for _, b := range deps.StaticBindings {
		if err = v.addBinding(b); err != nil {
			return nil, err
		}
	}
	keys := make([]store.Key, 0, len(cut.objects))
	for k := range cut.objects {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b store.Key) int { return compareKey(a, b) })
	for _, k := range keys {
		if b, ok := cut.objects[k].(*publicv1.RoleBinding); ok && b.DeletionTimestamp == nil {
			if err = v.addBinding(rbac.FromRoleBinding(b)); err != nil {
				return nil, err
			}
		}
	}
	for _, f := range cut.security.GrantFloors {
		if !v.knownGrant(f.Grant) {
			return nil, ErrInvalid
		}
		v.floors[f.Grant] = f.ValidAfter
	}
	for _, f := range cut.security.AccountFloors {
		v.accounts[f.Account] = f.ValidAfter
	}
	for _, f := range cut.security.ProviderFloors {
		v.providers[providerKey(f.Provider, f.UID)] = f.ValidAfter
	}
	for _, s := range cut.security.Sessions {
		for _, g := range s.Grants {
			if !v.knownGrant(g) {
				return nil, ErrInvalid
			}
		}
		s.Grants = slices.Clone(s.Grants)
		v.sessions[s.ID] = s
	}
	if err = v.prepareFactors(deps.TOTPKeys); err != nil {
		return nil, err
	}
	if err = v.prepareProviders(ctx, deps); err != nil {
		return nil, err
	}
	v.evidenceKeys, err = oidccore.NewEvidenceKeyring(deps.CurrentKey, deps.PreviousKey)
	if err != nil {
		return nil, ErrDependency
	}
	v.tokenSealer, err = session.NewSealer("external-api-token", deps.CurrentKey, deps.PreviousKey)
	if err != nil {
		return nil, ErrDependency
	}
	v.externalIdentities = map[AccountRef]ExternalIdentity{}
	for _, i := range cut.security.ExternalIdentities {
		a, _ := ExternalAccount(i)
		v.externalIdentities[a] = detachedExternal(i)
	}
	v.externalSessions = map[string]ExternalSessionRecord{}
	for _, record := range cut.security.ExternalSessions {
		record.Grants = slices.Clone(record.Grants)
		v.externalSessions[record.ID] = record
	}
	// Hash dependencies by their exact copied content as well as host cursor.
	v.identity = hash(struct {
		Cursor              string
		License             LicensePolicy
		Permissions         *rbac.Permissions
		Bindings            []rbac.Binding
		Current, Previous   string
		TOTPKeys            string
		Providers, Evidence string
	}{deps.Identity, deps.License, v.permissions, v.bindings, hash(deps.CurrentKey), hash(deps.PreviousKey), deps.TOTPKeys.Identity(), v.providerIdentity, v.evidenceKeys.Identity()})
	return v, nil
}

func (v *View) prepareFactors(keys *totp.Keyring) error {
	v.factors = map[totp.Account]*totp.Seed{}
	rows := map[totp.Account]totp.Record{}
	for _, row := range v.cut.security.TOTP {
		rows[row.Account] = row
		obj := v.cut.objects[store.Key{Kind: "LocalUser", Name: row.Account.Name}]
		current := obj != nil && string(obj.GetUID()) == row.Account.UID
		if row.Enabled && !current {
			return ErrInvalid
		}
		if !current {
			continue
		}
		user, ok := obj.(*internalv1.LocalUser)
		if !ok || row.Enabled != (user.Spec.TOTPSecret != "") {
			return ErrInvalid
		}
	}
	for _, obj := range v.cut.objects {
		user, ok := obj.(*internalv1.LocalUser)
		if !ok || user.Spec.TOTPSecret == "" {
			continue
		}
		a := totp.Account{Name: user.Name, UID: string(user.UID)}
		row, ok := rows[a]
		if !ok || !row.Enabled {
			return ErrManifest
		}
		if keys == nil {
			return ErrDependency
		}
		seed, err := keys.Open(a, user.Spec.TOTPSecret)
		if err != nil {
			return ErrInvalid
		}
		_, version, id := seed.Identity()
		if row.SeedVersion != version || row.SeedID != id {
			return ErrInvalid
		}
		v.factors[a] = seed
	}
	return nil
}
func compareKey(a, b store.Key) int {
	if a.String() < b.String() {
		return -1
	}
	if a.String() > b.String() {
		return 1
	}
	return 0
}
func (v *View) addBinding(b rbac.Binding) error {
	g := rbac.Grant{Role: b.Role, Scope: b.Scope}.Normalized()
	if !v.knownGrant(g.String()) || len(v.bindings) >= MaxRows || len(b.Subjects) > MaxRows {
		return ErrInvalid
	}
	b.Subjects = slices.Clone(b.Subjects)
	v.bindings = append(v.bindings, b)
	v.grants[g.String()] = true
	return nil
}
func (v *View) knownGrant(wire string) bool {
	if !validGrant(wire) {
		return false
	}
	g, _ := rbac.ParseGrant(wire)
	if _, ok := v.permissions.Roles[g.Role]; !ok {
		return false
	}
	if slices.Contains(v.permissions.Scoped, g.Role) && g.Scope.Kind == rbac.ScopeCluster {
		return false
	}
	switch g.Scope.Kind {
	case rbac.ScopeCluster, rbac.ScopeZone, rbac.ScopeZoneGroup, rbac.ScopeClientGroup:
		return true
	}
	return false
}

// LoadPrincipal refreshes ONLY captured accounts/private admin. It cannot prove
// credential possession or authorize a current command on its own.
func (v *View) LoadPrincipal(p auth.Principal, now time.Time) (auth.Principal, error) {
	if v != nil && !v.released.Load() && p.External != nil {
		return v.loadExternal(p, now)
	}
	if v == nil || v.released.Load() || p.AccountUID == "" || p.CredentialIssuedAt.IsZero() || p.CredentialIssuedAt.After(now) {
		return auth.Principal{}, ErrCredentials
	}
	var obj store.Object
	switch {
	case p.Kind == auth.KindUser && p.Provider == "local":
		obj = v.cut.objects[store.Key{Kind: "LocalUser", Name: p.Name}]
	case p.Kind == auth.KindServiceAccount && p.Provider == "kudurru":
		for k, candidate := range v.cut.objects {
			if k.Kind == "ServiceAccount" && k.Namespace+"/"+k.Name == p.ServiceAccount {
				obj = candidate
				break
			}
		}
	default:
		return auth.Principal{}, ErrUnsupported
	}
	if inactive(obj) || string(obj.GetUID()) != p.AccountUID {
		return auth.Principal{}, ErrCredentials
	}
	if p.CredentialIssuedAt.Before(obj.GetCreationTimestamp().Time) {
		return auth.Principal{}, ErrCredentials
	}
	p.BuiltinAdmin = false
	p.Implicit = nil
	p.Groups = nil
	p.MustChangePassword = false
	switch a := obj.(type) {
	case *internalv1.LocalUser:
		if p.ID != "local:"+a.Name {
			return auth.Principal{}, ErrCredentials
		}
		p.Groups = slices.Clone(a.Spec.Groups)
		// Existing local-user semantics refresh authority from account groups,
		// never from frozen grants of another subject's remaining binding.
		p.Grants = nil
		p.GrantsIssuedAt = time.Time{}
		p.BuiltinAdmin = a.Spec.BuiltinAdmin
		p.MustChangePassword = a.Spec.MustChangePassword
	case *publicv1.ServiceAccount:
		if p.ID != "sa:"+a.Namespace+"/"+a.Name {
			return auth.Principal{}, ErrCredentials
		}
		owner := internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: a.Name, Namespace: a.Namespace, UID: string(a.UID), ID: p.ID, Provider: "kudurru"}
		state, ok := v.cut.objects[store.Key{Kind: "PrincipalState", Name: internalv1.PrincipalStateName(owner)}].(*internalv1.PrincipalState)
		p.BuiltinAdmin = ok && state.DeletionTimestamp == nil && state.Spec.Owner == owner && state.Spec.BuiltinAdmin
	}
	return detachedPrincipal(p), nil
}

// Authorize uses only captured grants/scopes/private authority. It is a serving
// read decision; current commands must call ReauthorizeCurrent with raw proof.
func (v *View) Authorize(p auth.Principal, a rbac.Attributes, now time.Time) (rbac.Decision, error) {
	live, err := v.LoadPrincipal(p, now)
	if err != nil {
		return rbac.Decision{}, err
	}
	r := auth.RBACPrincipal(live)
	allow := func(grants []rbac.Grant) bool {
		for _, g := range grants {
			if g.Scope.Covers(a.Scope) && v.permissions.Grants(g.Role, a.Resource, a.Verb) {
				return true
			}
		}
		return false
	}
	denied := func(reason string) (rbac.Decision, error) {
		missing := a
		return rbac.Decision{Path: rbac.PathNone, Reason: reason, Missing: &missing}, nil
	}
	if len(r.Restrict) > 0 && !allow(r.Restrict) {
		return denied("outside token scopes")
	}
	if live.MustChangePassword {
		return denied("password change required")
	}
	if r.BuiltinAdmin {
		return rbac.Decision{Allowed: true, Path: rbac.PathLocal, Reason: "private builtin admin"}, nil
	}
	if !v.license.RBAC {
		return denied("management entitlement unavailable")
	}
	if live.External != nil {
		if allow(r.Grants) {
			return rbac.Decision{Allowed: true, Path: rbac.PathBinding, Reason: "captured external grant intersection"}, nil
		}
		return denied("external grant unavailable")
	}
	for _, b := range v.bindings {
		if b.Scope.Covers(a.Scope) && rbac.SubjectsMatch(b.Subjects, r) && v.permissions.Grants(b.Role, a.Resource, a.Verb) {
			return rbac.Decision{Allowed: true, Path: rbac.PathBinding, Reason: "captured binding"}, nil
		}
	}
	for _, g := range r.Grants {
		floor := v.floors[g.Normalized().String()]
		if v.grants[g.Normalized().String()] && (floor.IsZero() || live.GrantsIssuedAt.After(floor)) && allow([]rbac.Grant{g}) {
			return rbac.Decision{Allowed: true, Path: rbac.PathBinding, Reason: "enrolled grant"}, nil
		}
	}
	return denied("no captured grant")
}
