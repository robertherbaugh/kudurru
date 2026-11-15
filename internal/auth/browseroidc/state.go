// Package browseroidc implements bounded browser preauthentication data. It
// performs no I/O, durable admission, provider verification, or session issuance.
package browseroidc

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Protocol limits are fixed for this preauthentication format.
const (
	Purpose        = "oidc-browser-preauth/v2"
	FormatVersion  = 1
	Lifetime       = 10 * time.Minute
	MaxNextBytes   = 1024
	MaxCookieBytes = 3800
	MaxCodeBytes   = 4096
	maxPlainBytes  = 2500
)

// Safe errors never include state, nonce, codes, cookies, or upstream diagnostics.
var (
	ErrInvalid = errors.New("browseroidc: invalid preauthentication")
	ErrBounds  = errors.New("browseroidc: bounds exceeded")
	ErrTime    = errors.New("browseroidc: preauthentication time rejected")
)

type payload struct {
	Version   int       `json:"formatVersion"`
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	State     string    `json:"state"`
	Nonce     string    `json:"nonce"`
	Verifier  string    `json:"verifier"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Next      string    `json:"next"`
}

// State is authenticated private browser data. Its presence is not durable
// enrollment or single consumption; the command owner must require a pending row.
type State struct{ value payload }

func (State) String() string { return "private browser preauthentication" }

// GoString redacts private state during diagnostic formatting.
func (s State) GoString() string { return s.String() }

// MarshalJSON prevents generic serialization of private state.
func (State) MarshalJSON() ([]byte, error) { return []byte(`"private preauthentication"`), nil }

// New creates independent cryptographic state, nonce, verifier, and attempt ID.
// An empty next path means the application root. Now is the actual trusted clock.
func New(provider, next string, now time.Time) (State, error) {
	if len(provider) > 63 || len(validation.IsDNS1123Label(provider)) != 0 {
		return State{}, ErrInvalid
	}
	next, err := SafeNext(next)
	if err != nil {
		return State{}, err
	}
	if !validTime(now) || !validTime(now.Add(Lifetime)) {
		return State{}, ErrTime
	}
	s := State{payload{Version: FormatVersion, Provider: provider, Next: next, IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(Lifetime)}}
	for _, dest := range []*string{&s.value.ID, &s.value.State, &s.value.Nonce, &s.value.Verifier} {
		var random [32]byte
		_, _ = rand.Read(random[:])
		*dest = base64.RawURLEncoding.EncodeToString(random[:])
	}
	raw, _ := json.Marshal(s.value)
	defer clear(raw)
	if len(raw) > maxPlainBytes {
		return State{}, ErrBounds
	}
	return s, nil
}

func validTime(t time.Time) bool { return !t.IsZero() && t.Unix() >= 0 && t.Year() <= 9999 }

func randomValue(s string) bool {
	if len(s) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == 32
}

func (s State) validate() error {
	p := s.value
	if p.Version != FormatVersion || len(p.Provider) > 63 || len(validation.IsDNS1123Label(p.Provider)) != 0 || !randomValue(p.ID) || !randomValue(p.State) || !randomValue(p.Nonce) || !randomValue(p.Verifier) {
		return ErrInvalid
	}
	if !validTime(p.IssuedAt) || !validTime(p.ExpiresAt) || !p.ExpiresAt.Equal(p.IssuedAt.Add(Lifetime)) || p.IssuedAt.Location() != time.UTC || p.ExpiresAt.Location() != time.UTC {
		return ErrTime
	}
	next, err := SafeNext(p.Next)
	if err != nil || next != p.Next {
		return ErrInvalid
	}
	return nil
}

// ValidAt rejects expiry equality and clock regression. It performs no mutation.
func (s State) ValidAt(now time.Time) error {
	if err := s.validate(); err != nil {
		return err
	}
	if !validTime(now) || now.Before(s.value.IssuedAt) || !now.Before(s.value.ExpiresAt) {
		return ErrTime
	}
	return nil
}

// ID identifies the required durable pending attempt.
func (s State) ID() string { return s.value.ID }

// Provider is the exact route provider ID authenticated in the cookie.
func (s State) Provider() string { return s.value.Provider }

// Next is the validated same-origin destination.
func (s State) Next() string { return s.value.Next }

// IssuedAt is the original actual issuance time.
func (s State) IssuedAt() time.Time { return s.value.IssuedAt }

// ExpiresAt is the absolute preauthentication deadline.
func (s State) ExpiresAt() time.Time { return s.value.ExpiresAt }

// Nonce exposes the expected value only for explicit trusted JWT verification.
func (s State) Nonce() string { return s.value.Nonce }

// Verifier exposes the secret only for the explicit captured code exchange.
func (s State) Verifier() string { return s.value.Verifier }

// OAuthState exposes the state only for the explicit authorization redirect.
func (s State) OAuthState() string { return s.value.State }

// PayloadHash binds the durable pending row to canonical authenticated plaintext.
// A zero or malformed value has no hash. This does not authorize row creation.
func (s State) PayloadHash() string {
	if s.validate() != nil {
		return ""
	}
	raw, _ := json.Marshal(s.value)
	defer clear(raw)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MatchState checks a bounded returned OAuth state in constant time.
func (s State) MatchState(got string) bool {
	return s.validate() == nil && len(got) == 43 && subtle.ConstantTimeCompare([]byte(got), []byte(s.value.State)) == 1
}

// S256 implements RFC 7636 with strict 43..128 unreserved ASCII verifiers.
func S256(verifier string) (string, error) {
	if len(verifier) < 43 || len(verifier) > 128 {
		return "", ErrBounds
	}
	for _, c := range []byte(verifier) {
		allowed := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
		if !allowed {
			return "", ErrInvalid
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
