package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// Defaults from dev-docs/design-record.md §6.2 (auth.session) and §8.2.
const (
	DefaultCookieName    = "kudurru_session"
	DefaultIdleTimeout   = 30 * time.Minute
	DefaultLifetime      = 12 * time.Hour
	DefaultTouchInterval = time.Minute
	// MaxCookieBytes is the 4 KiB guard: len(name) + len("=") + len(value)
	// must not exceed it. Browsers cap name+value at 4096 bytes and silently
	// drop larger cookies, which would look like a login loop to the user;
	// a hard error at Issue time is the only honest failure.
	MaxCookieBytes = 4096
)

// payloadV1 is the schema version inside the sealed envelope.
const payloadV1 = 1

// Errors returned by Sealed. All of them mean "treat the request as
// unauthenticated"; they are distinct so logs and metrics can tell a
// tampered cookie from an expired or revoked one.
var (
	ErrExpired     = errors.New("session: absolute lifetime exceeded")
	ErrIdleExpired = errors.New("session: idle timeout exceeded")
	ErrRevoked     = errors.New("session: issued before the principal's sessionsValidAfter")
	ErrTooLarge    = errors.New("session: sealed cookie exceeds the 4 KiB guard")
)

// ValidAfterFunc returns the principal's sessionsValidAfter (zero when
// unset). It is consulted on every Load and must be cheap: production wires
// it to the store snapshot, never to a store round trip. Sessions issued at
// or before the returned instant are rejected with ErrRevoked; because iat
// has one-second resolution, a login in the same second as a revocation is
// also rejected (fail closed).
type ValidAfterFunc func(principalID string) time.Time

// Options configures a Sealed store.
type Options struct {
	// Key is the 32-byte KUDURRU_ENCRYPTION_KEY. Required.
	Key []byte
	// PreviousKey is the optional 32-byte KUDURRU_ENCRYPTION_KEY_PREVIOUS
	// (D57): cookies sealed under it still open; new seals use Key.
	PreviousKey []byte
	// CookieName is used by the 4 KiB guard. Default kudurru_session.
	CookieName string
	// IdleTimeout is the sliding idle limit. Default 30m.
	IdleTimeout time.Duration
	// Lifetime is the absolute session lifetime; Touch never extends past
	// it. Default 12h.
	Lifetime time.Duration
	// TouchInterval bounds how often Touch re-seals. Default 1m.
	TouchInterval time.Duration
	// ValidAfter is the sessionsValidAfter hook; nil disables the check.
	ValidAfter ValidAfterFunc
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Sealed is the v0.1 SessionStore: the session lives entirely in an
// AES-256-GCM sealed cookie. It is safe for concurrent use.
type Sealed struct {
	sealer *Sealer
	opts   Options
}

var _ auth.SessionStore = (*Sealed)(nil)

// NewSealed validates opts and builds the store.
func NewSealed(opts Options) (*Sealed, error) {
	sealer, err := NewSealer(PurposeSession, opts.Key, opts.PreviousKey)
	if err != nil {
		return nil, err
	}
	if opts.CookieName == "" {
		opts.CookieName = DefaultCookieName
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.Lifetime == 0 {
		opts.Lifetime = DefaultLifetime
	}
	if opts.TouchInterval == 0 {
		opts.TouchInterval = DefaultTouchInterval
	}
	if opts.IdleTimeout < 0 || opts.Lifetime < 0 || opts.TouchInterval < 0 {
		return nil, errors.New("session: durations must be positive")
	}
	if opts.IdleTimeout > opts.Lifetime {
		return nil, fmt.Errorf("session: idle timeout %s exceeds lifetime %s", opts.IdleTimeout, opts.Lifetime)
	}
	if opts.TouchInterval >= opts.IdleTimeout {
		return nil, fmt.Errorf("session: touch interval %s must be below the idle timeout %s", opts.TouchInterval, opts.IdleTimeout)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sealed{sealer: sealer, opts: opts}, nil
}

// Lifetime returns the configured absolute lifetime.
func (st *Sealed) Lifetime() time.Duration { return st.opts.Lifetime }

// IdleTimeout returns the configured idle timeout.
func (st *Sealed) IdleTimeout() time.Duration { return st.opts.IdleTimeout }

// CookieName returns the cookie name the 4 KiB guard accounts for.
func (st *Sealed) CookieName() string { return st.opts.CookieName }

type principalV1 struct {
	Kind       auth.PrincipalKind `json:"kind"`
	ID         string             `json:"id"`
	Name       string             `json:"name,omitempty"`
	Provider   string             `json:"provider,omitempty"`
	AccountUID string             `json:"accountUID,omitempty"`
}

// sessionV1 is the sealed payload. Field names follow §8.2 so a decrypted
// cookie is self-describing in a debugger. Principal.Groups is deliberately
// absent (D48): RolesSnapshot carries the resolved grants instead.
type sessionV1 struct {
	V             int         `json:"v"`
	SID           string      `json:"sid"`
	Principal     principalV1 `json:"principal"`
	RolesSnapshot []string    `json:"roles_snapshot,omitempty"`
	IDP           string      `json:"idp,omitempty"`
	IAT           int64       `json:"iat"`
	EXP           int64       `json:"exp"`
	IdleEXP       int64       `json:"idle_exp"`
	CSRF          string      `json:"csrf"`
}

// Issue seals s into a cookie value. Missing fields are filled in on a
// local copy: a random 128-bit ID, a random 256-bit CSRF token, IssuedAt =
// now, ExpiresAt = IssuedAt + Lifetime, IdleExpiresAt = now + IdleTimeout.
// A caller-supplied ExpiresAt is honoured but capped at IssuedAt +
// Lifetime. Callers that need the generated values use NewSession first.
// ErrTooLarge is returned when the cookie would exceed the 4 KiB guard.
func (st *Sealed) Issue(_ context.Context, s auth.Session) (string, error) {
	st.fill(&s)
	if s.Principal.ID == "" || s.Principal.Kind == "" {
		return "", errors.New("session: principal kind and id are required")
	}
	return st.seal(s)
}

// NewSession returns a fully populated session for p, ready for Issue, so
// the caller knows the session ID and CSRF token it is about to set.
func (st *Sealed) NewSession(p auth.Principal, roles []string, idp string) auth.Session {
	s := auth.Session{Principal: p, RolesSnapshot: roles, IDP: idp}
	st.fill(&s)
	return s
}

func (st *Sealed) fill(s *auth.Session) {
	now := st.opts.Now()
	if s.ID == "" {
		s.ID = randomToken(16)
	}
	if s.CSRF == "" {
		s.CSRF = randomToken(32)
	}
	if s.IssuedAt.IsZero() {
		s.IssuedAt = now
	}
	maxExp := s.IssuedAt.Add(st.opts.Lifetime)
	if s.ExpiresAt.IsZero() || s.ExpiresAt.After(maxExp) {
		s.ExpiresAt = maxExp
	}
	if s.IdleExpiresAt.IsZero() {
		s.IdleExpiresAt = now.Add(st.opts.IdleTimeout)
	}
	if s.IdleExpiresAt.After(s.ExpiresAt) {
		s.IdleExpiresAt = s.ExpiresAt
	}
	s.Principal.Groups = nil
}

func (st *Sealed) seal(s auth.Session) (string, error) {
	pt, err := json.Marshal(sessionV1{
		V:   payloadV1,
		SID: s.ID,
		Principal: principalV1{
			Kind:       s.Principal.Kind,
			ID:         s.Principal.ID,
			Name:       s.Principal.Name,
			Provider:   s.Principal.Provider,
			AccountUID: s.Principal.AccountUID,
		},
		RolesSnapshot: s.RolesSnapshot,
		IDP:           s.IDP,
		IAT:           s.IssuedAt.Unix(),
		EXP:           s.ExpiresAt.Unix(),
		IdleEXP:       s.IdleExpiresAt.Unix(),
		CSRF:          s.CSRF,
	})
	if err != nil {
		return "", fmt.Errorf("session: encode: %w", err)
	}
	if n := len(st.opts.CookieName) + 1 + SealedLen(len(pt)); n > MaxCookieBytes {
		return "", fmt.Errorf("%w (%d bytes, %d roles in snapshot)", ErrTooLarge, n, len(s.RolesSnapshot))
	}
	return st.sealer.Seal(pt), nil
}

// Load opens cookieValue and enforces, in order: seal integrity (current
// or previous key), absolute expiry, idle expiry, and sessionsValidAfter.
// The returned Principal has no Groups (D48).
func (st *Sealed) Load(_ context.Context, cookieValue string) (auth.Session, error) {
	if len(cookieValue) > MaxCookieBytes {
		return auth.Session{}, ErrInvalid
	}
	pt, _, err := st.sealer.Open(cookieValue)
	if err != nil {
		return auth.Session{}, err
	}
	var p sessionV1
	if err := json.Unmarshal(pt, &p); err != nil || p.V != payloadV1 || p.SID == "" || p.Principal.ID == "" {
		// Authenticated but unreadable means a future or broken writer
		// holding our key; never half-trust it.
		return auth.Session{}, ErrInvalid
	}
	s := auth.Session{
		ID: p.SID,
		Principal: auth.Principal{
			Kind:       p.Principal.Kind,
			ID:         p.Principal.ID,
			Name:       p.Principal.Name,
			Provider:   p.Principal.Provider,
			AccountUID: p.Principal.AccountUID,
		},
		RolesSnapshot: p.RolesSnapshot,
		IDP:           p.IDP,
		IssuedAt:      time.Unix(p.IAT, 0),
		ExpiresAt:     time.Unix(p.EXP, 0),
		IdleExpiresAt: time.Unix(p.IdleEXP, 0),
		CSRF:          p.CSRF,
	}
	now := st.opts.Now()
	if !now.Before(s.ExpiresAt) {
		return auth.Session{}, ErrExpired
	}
	if !now.Before(s.IdleExpiresAt) {
		return auth.Session{}, ErrIdleExpired
	}
	if st.opts.ValidAfter != nil {
		if va := st.opts.ValidAfter(s.Principal.ID); !va.IsZero() && !s.IssuedAt.After(va) {
			return auth.Session{}, ErrRevoked
		}
	}
	return s, nil
}

// Touch slides the idle expiry to now + IdleTimeout (never past ExpiresAt)
// and re-seals, but only when that moves idle_exp by at least
// TouchInterval: an active user costs one Set-Cookie per minute, not one
// per request. Re-sealing always uses the current key, so sessions sealed
// under KUDURRU_ENCRYPTION_KEY_PREVIOUS migrate on their next touch.
func (st *Sealed) Touch(_ context.Context, s *auth.Session) (string, bool, error) {
	now := st.opts.Now()
	if !now.Before(s.ExpiresAt) {
		return "", false, ErrExpired
	}
	if !now.Before(s.IdleExpiresAt) {
		return "", false, ErrIdleExpired
	}
	next := now.Add(st.opts.IdleTimeout)
	if next.After(s.ExpiresAt) {
		next = s.ExpiresAt
	}
	if next.Sub(s.IdleExpiresAt) < st.opts.TouchInterval {
		return "", false, nil
	}
	updated := *s
	updated.IdleExpiresAt = next
	v, err := st.seal(updated)
	if err != nil {
		return "", false, err
	}
	*s = updated
	return v, true, nil
}

// Revoke is unsupported: a sealed cookie cannot be recalled. Set the
// principal's sessionsValidAfter instead.
func (st *Sealed) Revoke(context.Context, string) error { return auth.ErrUnsupported }

// List is unsupported for sealed sessions (no server-side record).
func (st *Sealed) List(context.Context, string) ([]auth.SessionInfo, error) {
	return nil, auth.ErrUnsupported
}

// randomToken returns n random bytes, base64url without padding.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}
