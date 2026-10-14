// Package auth holds principals, sessions and the authenticator chain
// (docs/architecture.md §8): the Principal and Session types, the
// Middleware that runs the Authenticate and LoadPrincipal steps of §8.8
// over pluggable Authenticator steps (implemented by internal/auth/tokens,
// ee/oidc, internal/auth/k8s, internal/auth/session and the
// unix-peer and internal-token steps here), CSRF, the contract's error
// body, the Authorize helper and the mapping to rbac.Principal.
package auth

import (
	"context"
	"errors"
	"time"
)

// PrincipalKind is who is acting.
type PrincipalKind string

// Principal kinds.
const (
	KindUser                     PrincipalKind = "User"
	KindServiceAccount           PrincipalKind = "ServiceAccount"
	KindKubernetesServiceAccount PrincipalKind = "KubernetesServiceAccount"
	KindLocalRoot                PrincipalKind = "LocalRoot"
	KindNode                     PrincipalKind = "Node"
)

// Principal is the authenticated identity. The identity key for OIDC users
// is (provider, issuer, subject), never email.
type Principal struct {
	Kind     PrincipalKind
	ID       string
	Name     string
	Groups   []string
	Provider string
	// AuthMethod is how the principal authenticated: cookie, token, jwt,
	// k8s, unix, internal.
	AuthMethod string

	// The fields below are filled per request by the authenticators and
	// LoadPrincipal (§8.8); sealed sessions persist none of them.

	// UID is the Kubernetes user UID of a KubernetesServiceAccount.
	UID string
	// ServiceAccount is "<namespace>/<name>" for kudurru and Kubernetes
	// service accounts; binding subjects of kind ServiceAccount match it.
	ServiceAccount string
	// BuiltinAdmin grants admin without a binding: the first-boot admin,
	// the bootstrap service account and the unix-socket peer.
	BuiltinAdmin bool
	// MustChangePassword restricts a local user to changing the password.
	MustChangePassword bool
	// Grants are pre-resolved grants in wire form ("role" or
	// "role@kind:name"): a cookie session's roles snapshot or the frozen
	// grants of an OIDC user's personal token, honoured while the
	// bindings still provide them (rbac.BindingSource).
	Grants []string
	// GrantsIssuedAt is when Grants were resolved.
	GrantsIssuedAt time.Time
	// Scopes, when non-empty, narrow every decision to these grants (API
	// token scopes, §8.6).
	Scopes []string
	// TokenID is the id of the API token that authenticated the request.
	TokenID string
	// CredentialIssuedAt is when the credential that authenticated the
	// request was issued (a session's iat, an API token's creation time).
	// LoadPrincipal refuses a credential older than the local account it
	// names, so a re-created account never inherits the sessions and
	// tokens of a deleted one with the same name. Zero skips the check.
	CredentialIssuedAt time.Time
	// Implicit are grants held without a binding (a peer node's read-only
	// grants), in wire form.
	Implicit []string
}

// Session is the payload of a sealed cookie or a db row. RolesSnapshot
// holds resolved bindings, never the raw IdP group list (decision D48).
type Session struct {
	ID            string
	Principal     Principal
	RolesSnapshot []string
	IDP           string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
	CSRF          string
}

// SessionInfo is what List returns for the UI.
type SessionInfo struct {
	ID        string
	IssuedAt  time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
	IP        string
	UserAgent string
}

// ErrUnsupported is returned by stores that cannot perform an operation
// (the sealed store cannot revoke or list; use sessionsValidAfter).
var ErrUnsupported = errors.New("auth: unsupported by this session store")

// SessionStore is implemented by sealed (v0.1) and db (v0.2).
type SessionStore interface {
	Issue(ctx context.Context, s Session) (cookieValue string, err error)
	Load(ctx context.Context, cookieValue string) (Session, error)
	Touch(ctx context.Context, s *Session) (cookieValue string, changed bool, err error)
	Revoke(ctx context.Context, sid string) error
	List(ctx context.Context, principal string) ([]SessionInfo, error)
}
