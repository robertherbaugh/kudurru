package api

import "time"

// The request and response shapes of the routes the auth stack serves
// (Options.AuthHandler, internal/auth/stack): /auth/* except /auth/me, and
// /iam/*. They mirror the stack's types field for field so the OpenAPI
// document and the clients generated from it match the wire;
// TestForwardedShapes (cmd/kudurru) compares the two sets and decodes the
// real answers strictly into these types.
type (
	// AuthStatus is GET /auth/status (stack.StatusResponse).
	AuthStatus struct {
		LocalLogin            bool               `json:"localLogin" doc:"False only when auth.localLogin is disabled."`
		LocalLoginMode        string             `json:"localLoginMode" enum:"enabled,admins-only,disabled" doc:"auth.localLogin."`
		Providers             []AuthProvider     `json:"providers" nullable:"false" doc:"The OIDC login options."`
		HasDefaultCredentials bool               `json:"hasDefaultCredentials" doc:"The bootstrap admin still has its generated password."`
		Session               *AuthSessionLimits `json:"session,omitempty"`
	}
	// AuthProvider is one OIDC login option.
	AuthProvider struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}
	// AuthSessionLimits are the cookie session timeouts.
	AuthSessionLimits struct {
		IdleTimeout string `json:"idleTimeout" doc:"Go duration (auth.session.idleTimeout)."`
		Lifetime    string `json:"lifetime" doc:"Go duration (auth.session.lifetime)."`
	}

	// LoginRequest is the body of POST /auth/login.
	LoginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTPCode string `json:"totpCode,omitempty" doc:"The enrolled authenticator code, when required."`
	}
	// SessionInfo is the answer of POST /auth/login and POST
	// /auth/password (stack.MeResponse): the new session's principal,
	// grants, CSRF token and permission matrix.
	SessionInfo struct {
		Principal SessionPrincipal `json:"principal"`
		Roles     []string         `json:"roles" nullable:"false" doc:"Effective grants: role, or role@kind:name for scoped grants."`
		CSRF      string           `json:"csrf,omitempty" doc:"The X-Kudurru-CSRF value of the session."`
		// Permissions are the cluster-scope verbs per resource;
		// ScopedPermissions add the verbs per scoped grant.
		Permissions        map[string][]string            `json:"permissions"`
		ScopedPermissions  map[string]map[string][]string `json:"scopedPermissions,omitempty" doc:"Verbs per resource for each scoped grant, keyed by scope (zone:home-example-com)."`
		MustChangePassword bool                           `json:"mustChangePassword,omitempty" doc:"Every route but the self-service ones answers 403 password_change_required until POST /auth/password."`
		ExpiresAt          *time.Time                     `json:"expiresAt,omitempty" doc:"End of the session lifetime."`
		IdleExpiresAt      *time.Time                     `json:"idleExpiresAt,omitempty" doc:"End of the session if idle."`
	}
	// SessionPrincipal is the principal of SessionInfo
	// (stack.MePrincipal).
	SessionPrincipal struct {
		Kind       string   `json:"kind" doc:"User, ServiceAccount, KubernetesServiceAccount, LocalRoot or Node."`
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Groups     []string `json:"groups" nullable:"false"`
		Provider   string   `json:"provider,omitempty" doc:"local, or the identity provider id."`
		AuthMethod string   `json:"authMethod,omitempty" doc:"cookie, token, jwt, k8s, ..."`
		TokenID    string   `json:"tokenID,omitempty"`
	}
	// PasswordChangeRequest is the body of POST /auth/password (local
	// users; clears mustChangePassword and revokes every other session).
	PasswordChangeRequest struct {
		CurrentPassword string `json:"currentPassword" doc:"The current password."`
		NewPassword     string `json:"newPassword" doc:"The new password (at least 12 characters, different from the current one)."`
		RevokeTokens    bool   `json:"revokeTokens,omitempty" doc:"Also revoke every API token of the user; a password change alone revokes sessions, not tokens."`
		TOTPCode        string `json:"totpCode,omitempty" doc:"The enrolled authenticator code, when required."`
	}
	// SessionRecordInfo is an enrollment belonging to the current account UID.
	// It never includes cookie secrets, CSRF hashes or authentication evidence.
	SessionRecordInfo struct {
		ID            string    `json:"id"`
		IssuedAt      time.Time `json:"issuedAt"`
		ExpiresAt     time.Time `json:"expiresAt"`
		IdleExpiresAt time.Time `json:"idleExpiresAt"`
		Current       bool      `json:"current"`
		Revoked       bool      `json:"revoked"`
	}

	// CreateTokenRequest is the body of POST /auth/tokens.
	CreateTokenRequest struct {
		Description string   `json:"description,omitempty" doc:"Free text shown in the token list."`
		ExpiresIn   string   `json:"expiresIn,omitempty" doc:"Go duration (720h); empty means auth.tokens.maxLifetime, or never."`
		Scopes      []string `json:"scopes,omitempty" doc:"Narrow the token to these grants in wire form (viewer, zone-admin@zone:home-example-com); empty keeps the owner's grants."`
	}
	// TokenInfo is an API token as listed (never the secret or its hash).
	TokenInfo struct {
		ID          string     `json:"id"`
		Description string     `json:"description,omitempty"`
		Owner       TokenOwner `json:"owner"`
		CreatedAt   time.Time  `json:"createdAt"`
		ExpiresAt   *time.Time `json:"expiresAt" nullable:"true" doc:"null: the token never expires."`
		LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
		LastUsedIP  string     `json:"lastUsedIP,omitempty"`
		Scopes      []string   `json:"scopes,omitempty"`
	}
	// TokenOwner is the user or service account a token belongs to.
	TokenOwner struct {
		Kind      string `json:"kind" enum:"User,ServiceAccount"`
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty" doc:"The service account's namespace."`
		ID        string `json:"id,omitempty" doc:"The owner's principal id (local:alice, oidc:...)."`
		Provider  string `json:"provider,omitempty" doc:"local, or the identity provider id."`
		UID       string `json:"uid,omitempty" doc:"Immutable account identity for local users and service accounts."`
	}
	// CreatedToken is the answer of POST /auth/tokens; Token, the kdr_
	// secret, is shown this once.
	CreatedToken struct {
		TokenInfo
		Token string `json:"token" doc:"The kdr_ secret, shown only in this answer."`
	}

	// IAMUser is one local user of GET /iam/users (adminsock.User).
	IAMUser struct {
		Name               string     `json:"name"`
		DisplayName        string     `json:"displayName,omitempty"`
		BuiltinAdmin       bool       `json:"builtinAdmin"`
		Disabled           bool       `json:"disabled"`
		MustChangePassword bool       `json:"mustChangePassword"`
		Groups             []string   `json:"groups,omitempty"`
		LastLoginAt        *time.Time `json:"lastLoginAt,omitempty"`
		PasswordChangedAt  *time.Time `json:"passwordChangedAt,omitempty"`
	}
	// ServiceAccountInfo is one kudurru service account of GET
	// /iam/serviceaccounts.
	ServiceAccountInfo struct {
		Namespace    string `json:"namespace"`
		Name         string `json:"name"`
		Description  string `json:"description,omitempty"`
		Disabled     bool   `json:"disabled"`
		BuiltinAdmin bool   `json:"builtinAdmin"`
		Tokens       int    `json:"tokens" doc:"Number of API tokens the account owns."`
	}
	// RoleBindingInfo is one binding of GET /iam/rolebindings: from the
	// config file (rbac.bindings) or a RoleBinding object.
	RoleBindingInfo struct {
		Source    string           `json:"source" enum:"config,object"`
		Namespace string           `json:"namespace,omitempty"`
		Name      string           `json:"name" doc:"The object name, or rbac.bindings[i] for config bindings."`
		Role      string           `json:"role"`
		Scope     BindingScope     `json:"scope"`
		Subjects  []BindingSubject `json:"subjects"`
		ManagedBy string           `json:"managedBy,omitempty"`
	}
	// BindingScope is where a binding grants its role.
	BindingScope struct {
		Kind string `json:"kind" doc:"cluster, zone, ..."`
		Name string `json:"name,omitempty"`
	}
	// BindingSubject is one subject of a binding.
	BindingSubject struct {
		Kind      string `json:"kind" doc:"User, Group, ServiceAccount, ..."`
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	}
	// IdentityProviderInfo is one configured OIDC provider of GET
	// /iam/identityproviders (no secrets).
	IdentityProviderInfo struct {
		ID          string   `json:"id"`
		DisplayName string   `json:"displayName"`
		Issuer      string   `json:"issuer"`
		ClientID    string   `json:"clientID"`
		CLIClientID string   `json:"cliClientID,omitempty"`
		Scopes      []string `json:"scopes"`
		RedirectURL string   `json:"redirectURL"`
		Source      string   `json:"source" enum:"config,object"`
	}
)

// AuthList is the list envelope of the token and IAM routes the auth stack
// serves. They are not paged: continue is always empty.
type AuthList[T any] struct {
	Items    []T    `json:"items" nullable:"false"`
	Continue string `json:"continue"`
	TookMS   int64  `json:"took_ms"`
	// Diagnoses explain ignored entries (role bindings, identity
	// providers).
	Diagnoses []string `json:"diagnoses,omitempty" doc:"Why entries were ignored (role bindings, identity providers)."`
}
