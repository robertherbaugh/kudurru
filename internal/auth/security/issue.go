package security

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Enrollment is returned only after staging a current local login. The caller
// must commit its command and acquire a complete serving view containing the
// enrollment before Seal. It carries no independently usable cookie.
type Enrollment struct {
	id, csrf string
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

// IssueLocal verifies the password, exact account UID, current licensed grants
// and local-login policy inside the supplied command. It derives lifetime from
// canonical settings, generates fresh identifiers, and stages the enrollment.
// Password-only login to a TOTP account remains unavailable. The caller must
// return any error from its Command callback; dependency freshness and shared
// lockout/TOTP admission belong to that command's outer gate.
func IssueLocal(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, now time.Time) (Enrollment, error) {
	d, err := servingauth.ResolveSessionCurrent(ctx, tx, deps, proof, now)
	if err != nil {
		return Enrollment{}, err
	}
	if d.Principal.Kind != auth.KindUser || d.Principal.Provider != "local" || d.Principal.AccountUID == "" || d.Settings == nil {
		return Enrollment{}, servingauth.ErrUnsupported
	}
	cfg, err := localLoginConfig(d)
	if err != nil {
		return Enrollment{}, err
	}
	if now.IsZero() || cfg.Auth.Session.Lifetime.Std() <= 0 || cfg.Auth.Session.IdleTimeout.Std() <= 0 {
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
		Provider: "local", IssuedAt: now.UTC(), ExpiresAt: now.Add(cfg.Auth.Session.Lifetime.Std()).UTC(), IdleExpiresAt: now.Add(cfg.Auth.Session.IdleTimeout.Std()).UTC(),
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

func localLoginConfig(d servingauth.SessionDecision) (*config.Config, error) {
	cfg, err := sessionConfig(d.Settings)
	if err != nil {
		return nil, err
	}
	if cfg.Auth.LocalLogin == config.LocalLoginDisabled || cfg.Auth.LocalLogin == config.LocalLoginAdminsOnly && !d.Principal.BuiltinAdmin {
		return nil, servingauth.ErrDenied
	}
	return cfg, nil
}

func sessionConfig(settings *publicv1.Kudurru) (*config.Config, error) {
	if settings == nil {
		return nil, servingauth.ErrInvalid
	}
	raw, err := json.Marshal(settings.Spec.Settings)
	if err != nil {
		return nil, servingauth.ErrInvalid
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, servingauth.ErrInvalid
	}
	defaults, err := config.FromFlat(config.Defaults())
	if err != nil {
		return nil, err
	}
	cfg, _, err := config.ApplyClusterDocument(defaults, document)
	if err != nil {
		return nil, servingauth.ErrInvalid
	}
	return cfg, nil
}

func randomID() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read either fills the buffer or terminates.
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
