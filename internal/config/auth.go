package config

import (
	"errors"
	"fmt"
	"time"
)

// Auth configures authentication (dev-docs/design-record.md §6.2, §8).
type Auth struct {
	// LocalLogin is enabled, admins-only or disabled; it gates POST
	// /api/v1/auth/login. The admin socket always works (break-glass).
	LocalLogin string        `koanf:"localLogin" json:"localLogin"`
	Bootstrap  AuthBootstrap `koanf:"bootstrap" json:"bootstrap"`
	Session    AuthSession   `koanf:"session" json:"session"`
	Tokens     AuthTokens    `koanf:"tokens" json:"tokens"`
	// IdentityProviders are OpenID Providers in the IdentityProvider spec
	// shape plus an "id" key (ee/oidc.ProviderFromMap).
	IdentityProviders []map[string]any `koanf:"identityProviders" json:"identityProviders,omitempty"`
	Kubernetes        AuthKubernetes   `koanf:"kubernetes" json:"kubernetes"`
	AdminSocket       AuthAdminSocket  `koanf:"adminSocket" json:"adminSocket"`
}

// AuthBootstrap configures the first-boot admin.
type AuthBootstrap struct {
	// AdminPasswordFile holds the first-boot admin password; the
	// KUDURRU_ADMIN_PASSWORD[_FILE] environment variables take precedence.
	AdminPasswordFile string `koanf:"adminPasswordFile" json:"adminPasswordFile"`
}

// AuthSession configures sealed session cookies.
type AuthSession struct {
	IdleTimeout Duration `koanf:"idleTimeout" json:"idleTimeout"`
	Lifetime    Duration `koanf:"lifetime" json:"lifetime"`
	// Secure is auto, always or never (the cookie Secure attribute).
	Secure string `koanf:"secure" json:"secure"`
}

// AuthTokens configures API tokens.
type AuthTokens struct {
	// MaxLifetime caps token expiry; 0 allows tokens without expiry.
	MaxLifetime Duration `koanf:"maxLifetime" json:"maxLifetime"`
}

// AuthKubernetes configures how Kubernetes identities are authorized.
type AuthKubernetes struct {
	// Mode is sar (SubjectAccessReview, D55). impersonate is v0.2.
	Mode string `koanf:"mode" json:"mode"`
}

// AuthAdminSocket is the local admin socket (`kudurru admin`, §8.7).
type AuthAdminSocket struct {
	// Path of the unix socket; empty disables it.
	Path string `koanf:"path" json:"path"`
	// Group is a gid whose members may use the socket besides uid 0 and
	// the server's own uid; -1 for none.
	Group int `koanf:"group" json:"group"`
}

// RBAC holds role bindings declared in the config file (§8.5).
type RBAC struct {
	// Bindings are {role, scope{kind,name}, subjects[{kind,name,namespace}]}
	// maps (internal/rbac.ParseBindings).
	Bindings []map[string]any `koanf:"bindings" json:"bindings,omitempty"`
}

// Local login modes.
const (
	LocalLoginEnabled    = "enabled"
	LocalLoginAdminsOnly = "admins-only"
	LocalLoginDisabled   = "disabled"
)

// validate checks the auth keys the metadata cannot express. The identity
// provider and binding entries get a shallow check from the metadata and
// a full one when ee/oidc and internal/rbac build them.
func (a *Auth) validate() []error {
	var errs []error
	if a.Session.IdleTimeout.Std() < time.Minute {
		errs = append(errs, fmt.Errorf("auth.session.idleTimeout: %s is shorter than 1m", a.Session.IdleTimeout))
	}
	if a.Session.Lifetime.Std() < a.Session.IdleTimeout.Std() {
		errs = append(errs, fmt.Errorf("auth.session.lifetime (%s) is shorter than auth.session.idleTimeout (%s)", a.Session.Lifetime, a.Session.IdleTimeout))
	}
	if a.Tokens.MaxLifetime < 0 {
		errs = append(errs, fmt.Errorf("auth.tokens.maxLifetime: %s is negative", a.Tokens.MaxLifetime))
	}
	if a.Kubernetes.Mode == "impersonate" {
		errs = append(errs, errors.New("auth.kubernetes.mode: impersonate is planned for v0.2 (D55: the impersonate grant makes kudurru cluster-admin-equivalent); v0.1 supports only sar"))
	}
	seen := map[string]bool{}
	for i, p := range a.IdentityProviders {
		id, _ := p["id"].(string)
		if id != "" && seen[id] {
			errs = append(errs, fmt.Errorf("auth.identityProviders[%d]: id %q is defined twice", i, id))
		}
		seen[id] = true
	}
	return errs
}
