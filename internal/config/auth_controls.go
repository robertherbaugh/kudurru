package config

import (
	"errors"
	"slices"
	"time"
)

// AuthControls is immutable policy for new credentials and HTTP cookie names.
// It never changes a previously issued credential's absolute deadlines.
type AuthControls struct {
	LocalLogin         string
	SessionIdleTimeout time.Duration
	SessionLifetime    time.Duration
	SessionSecure      string
	TokenMaxLifetime   time.Duration
}

// AuthControlOverrides contains only installed host environment locks. It owns
// no maps, configuration pointers, credentials or resource acquisition handles.
type AuthControlOverrides struct {
	Values              AuthControls
	LocalLoginLocked    bool
	SessionIdleLocked   bool
	SessionLifeLocked   bool
	SessionSecureLocked bool
	TokenLifeLocked     bool
}

// CaptureAuthOverrides detaches the five supported host environment locks.
func CaptureAuthOverrides(host *Config) AuthControlOverrides {
	var out AuthControlOverrides
	if host == nil {
		return out
	}
	out.LocalLoginLocked = slices.Contains(host.EnvLocked, "auth.localLogin")
	out.SessionIdleLocked = slices.Contains(host.EnvLocked, "auth.session.idleTimeout")
	out.SessionLifeLocked = slices.Contains(host.EnvLocked, "auth.session.lifetime")
	out.SessionSecureLocked = slices.Contains(host.EnvLocked, "auth.session.secure")
	out.TokenLifeLocked = slices.Contains(host.EnvLocked, "auth.tokens.maxLifetime")
	if out.LocalLoginLocked {
		out.Values.LocalLogin = host.Auth.LocalLogin
	}
	if out.SessionIdleLocked {
		out.Values.SessionIdleTimeout = host.Auth.Session.IdleTimeout.Std()
	}
	if out.SessionLifeLocked {
		out.Values.SessionLifetime = host.Auth.Session.Lifetime.Std()
	}
	if out.SessionSecureLocked {
		out.Values.SessionSecure = host.Auth.Session.Secure
	}
	if out.TokenLifeLocked {
		out.Values.TokenMaxLifetime = host.Auth.Tokens.MaxLifetime.Std()
	}
	return out
}

// Validate checks each lock independently; cross-field relationships are checked
// only after the valid canonical values and installed locks have been combined.
func (o AuthControlOverrides) Validate() error {
	v := o.Values
	if o.LocalLoginLocked && !validLocalLogin(v.LocalLogin) || !o.LocalLoginLocked && v.LocalLogin != "" ||
		o.SessionSecureLocked && !validSessionSecure(v.SessionSecure) || !o.SessionSecureLocked && v.SessionSecure != "" ||
		o.SessionIdleLocked && v.SessionIdleTimeout < time.Minute || !o.SessionIdleLocked && v.SessionIdleTimeout != 0 ||
		o.SessionLifeLocked && v.SessionLifetime < time.Minute || !o.SessionLifeLocked && v.SessionLifetime != 0 ||
		v.TokenMaxLifetime < 0 || !o.TokenLifeLocked && v.TokenMaxLifetime != 0 {
		return errors.New("auth controls: invalid host override")
	}
	return nil
}

// ResolveAuthControls validates the ENTIRE canonical document before applying
// locks. Invalid canonical input cannot be hidden by a local override.
func ResolveAuthControls(document map[string]any, overrides AuthControlOverrides) (AuthControls, error) {
	if err := overrides.Validate(); err != nil {
		return AuthControls{}, err
	}
	defaults, err := FromFlat(Defaults())
	if err != nil {
		return AuthControls{}, err
	}
	cfg, _, err := ApplyClusterDocument(defaults, document)
	if err != nil {
		return AuthControls{}, err
	}
	out := AuthControls{LocalLogin: cfg.Auth.LocalLogin, SessionIdleTimeout: cfg.Auth.Session.IdleTimeout.Std(), SessionLifetime: cfg.Auth.Session.Lifetime.Std(), SessionSecure: cfg.Auth.Session.Secure, TokenMaxLifetime: cfg.Auth.Tokens.MaxLifetime.Std()}
	v := overrides.Values
	if overrides.LocalLoginLocked {
		out.LocalLogin = v.LocalLogin
	}
	if overrides.SessionIdleLocked {
		out.SessionIdleTimeout = v.SessionIdleTimeout
	}
	if overrides.SessionLifeLocked {
		out.SessionLifetime = v.SessionLifetime
	}
	if overrides.SessionSecureLocked {
		out.SessionSecure = v.SessionSecure
	}
	if overrides.TokenLifeLocked {
		out.TokenMaxLifetime = v.TokenMaxLifetime
	}
	if !validLocalLogin(out.LocalLogin) || !validSessionSecure(out.SessionSecure) || out.SessionIdleTimeout < time.Minute || out.SessionLifetime < out.SessionIdleTimeout || out.TokenMaxLifetime < 0 {
		return AuthControls{}, errors.New("auth controls: invalid effective policy")
	}
	return out, nil
}

func validLocalLogin(s string) bool {
	return s == LocalLoginEnabled || s == LocalLoginAdminsOnly || s == LocalLoginDisabled
}
func validSessionSecure(s string) bool { return s == "auto" || s == "always" || s == "never" }

// AuthControlHot reports only the five controls mounted by complete serving.
// Global schema metadata remains conservative for other runtime adapters.
func AuthControlHot(path string) bool {
	switch path {
	case "auth.localLogin", "auth.session.idleTimeout", "auth.session.lifetime", "auth.session.secure", "auth.tokens.maxLifetime":
		return true
	}
	return false
}

// RequiresRestart retains startup semantics unless the runtime explicitly owns
// the complete immutable auth-control lifecycle.
func RequiresRestart(k Key, completeAuth bool) bool {
	return k.RestartRequired && (!completeAuth || !AuthControlHot(k.Path))
}

// RestartKeys classifies paths with the same runtime predicate as schema reads.
func RestartKeys(paths []string, completeAuth bool) []string {
	var out []string
	for _, path := range paths {
		if k, ok := Lookup(path); ok && RequiresRestart(k, completeAuth) {
			out = append(out, path)
		}
	}
	return out
}
