package security

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Enrollment is returned only after staging a current local login. The caller
// must commit its command and acquire a complete serving view containing the
// enrollment before Seal. It carries no independently usable cookie.
type Enrollment struct {
	id, csrf   string
	logoutHint browseroidc.Logout
}

func (Enrollment) String() string { return "private session enrollment" }

// GoString redacts enrollment secrets from Go diagnostics.
func (e Enrollment) GoString() string { return e.String() }

// MarshalJSON prevents the CSRF secret from entering structured diagnostics.
func (Enrollment) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// ID is the random public session identifier, without its CSRF proof.
func (e Enrollment) ID() string { return e.id }

// Seal verifies the enrollment against the acquired owner view after sealing.
// A revoked/replaced/expired account or a view missing the commit cannot issue
// a response cookie. The returned CSRF secret belongs only in the auth response.
func (e Enrollment) Seal(view *servingauth.View, now time.Time) (string, auth.Session, error) {
	if view == nil || e.id == "" || e.csrf == "" {
		return "", auth.Session{}, servingauth.ErrInvalid
	}
	cookie, err := view.SealSession(e.id, e.csrf)
	if err != nil {
		return "", auth.Session{}, err
	}
	s, err := view.CookieSession(cookie, now)
	if err != nil {
		return "", auth.Session{}, err
	}
	return cookie, s, nil
}

// SealLogoutHint releases only encrypted bounded hint evidence after the exact
// enrollment is visible and currently authenticates in the complete owner view.
func (e Enrollment) SealLogoutHint(view *servingauth.View, now time.Time) (string, time.Time, error) {
	if _, _, err := e.Seal(view, now); err != nil {
		return "", time.Time{}, err
	}
	raw, err := view.SealBrowserLogout(e.logoutHint, now)
	return raw, e.logoutHint.ExpiresAt(), err
}

// IssueLocal verifies the password, exact account UID, current licensed grants
// and local-login policy inside the supplied command. It derives lifetime from
// canonical settings, generates fresh identifiers, and stages the enrollment.
// Password-only login to a TOTP account remains unavailable. The caller must
// return any error from its Command callback; dependency freshness and shared
// lockout/TOTP admission belong to that command's outer gate.
// IssueLocalWithAdmission is the dedicated wrapper that can retain a typed
// pre-mutation credential refusal while committing only password counters.
func IssueLocal(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, now time.Time) (Enrollment, error) {
	d, err := servingauth.ResolveSessionCurrent(ctx, tx, deps, proof, now)
	if err != nil {
		return Enrollment{}, err
	}
	return issueLocalDecision(ctx, tx, d, now)
}

// issueLocalDecision is private: callers cannot mint an enrollment from a
// claimed principal. Its callers obtain d by actual password/factor verification
// in this same command, before their own local password mutation if any.
func issueLocalDecision(ctx context.Context, tx *sqlite.CommandTx, d servingauth.SessionDecision, now time.Time) (Enrollment, error) {
	if d.Principal.Kind != auth.KindUser || d.Principal.Provider != "local" || d.Principal.AccountUID == "" || d.Settings == nil {
		return Enrollment{}, servingauth.ErrUnsupported
	}
	cfg, err := localLoginControls(d)
	if err != nil {
		return Enrollment{}, err
	}
	if now.IsZero() || cfg.SessionLifetime <= 0 || cfg.SessionIdleTimeout <= 0 {
		return Enrollment{}, servingauth.ErrInvalid
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return Enrollment{}, err
	}
	if err := s.CollectExpired(now); err != nil {
		return Enrollment{}, err
	}
	id, csrf := randomID(), randomID()
	_, err = s.Enroll(servingauth.SessionRecord{
		ID: id, Account: servingauth.AccountRef{Kind: "LocalUser", Name: d.Principal.Name, UID: d.Principal.AccountUID},
		Provider: "local", IssuedAt: now.UTC(), ExpiresAt: now.Add(cfg.SessionLifetime).UTC(), IdleExpiresAt: now.Add(cfg.SessionIdleTimeout).UTC(),
		Grants: d.Grants, CSRFHash: servingauth.CSRFHash(csrf),
	})
	if err != nil {
		return Enrollment{}, err
	}
	if err := s.Commit(ctx); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{id: id, csrf: csrf}, nil
}

func localLoginControls(d servingauth.SessionDecision) (config.AuthControls, error) {
	cfg := d.Controls
	// ResolveSessionCurrent supplies normalized grants after current license
	// policy. Only the exact cluster admin grant permits admins-only login;
	// scoped admin grants and unlicensed bindings cannot broaden this gate.
	admin := d.Principal.BuiltinAdmin || slices.Contains(d.Grants, "admin")
	if cfg.LocalLogin == config.LocalLoginDisabled || cfg.LocalLogin == config.LocalLoginAdminsOnly && !admin {
		return config.AuthControls{}, servingauth.ErrDenied
	}
	return cfg, nil
}

func randomID() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read either fills the buffer or terminates.
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
