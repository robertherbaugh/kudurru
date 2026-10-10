// Package auth holds principals, sessions and the authenticator chain
// (docs/architecture.md §8). Only the seams exist in the scaffold.
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
