// Package oidccore implements bounded offline OIDC proof verification and pure
// claim policy. It performs no discovery, I/O, authorization or enrollment.
package oidccore

import (
	"errors"
	"time"
)

// Safe diagnostic categories never include credentials, claims or key documents.
var (
	ErrInvalid   = errors.New("oidccore: invalid input")
	ErrBounds    = errors.New("oidccore: input bounds exceeded")
	ErrSignature = errors.New("oidccore: signature rejected")
	ErrClaims    = errors.New("oidccore: claims rejected")
	ErrTime      = errors.New("oidccore: proof time rejected")
	ErrAudience  = errors.New("oidccore: audience rejected")
	ErrPolicy    = errors.New("oidccore: policy rejected")
	ErrBrowser   = errors.New("oidccore: browser proof rejected")
)

// Hard limits precede JSON/key/compiler allocations. No limit causes truncation.
const (
	MaxTokenBytes   = 64 << 10
	MaxHeaderBytes  = 4 << 10
	MaxClaimsBytes  = 32 << 10
	MaxJWKSBytes    = 256 << 10
	MaxKeys         = 64
	MaxJSONDepth    = 16
	MaxJSONNodes    = 4096
	MaxJSONMembers  = 1024
	MaxStringBytes  = 4096
	MaxIssuerBytes  = 2048
	MaxSubjectBytes = 512
	MaxAudiences    = 66
	MaxGroups       = 256
	MaxNameBytes    = 512
	MaxPolicyBytes  = 64 << 10
)

// ProofMode selects a compiled audience policy, never weaker crypto/time checks.
type ProofMode uint8

// Supported proof paths cannot bypass signature/time verification.
const (
	BearerIDToken ProofMode = iota + 1
	BrowserIDToken
)

// BrowserProof contains the expected preauth nonce and exchanged access token.
// Root validates and consumes preauth state and performs the exchange separately.
type BrowserProof struct{ Nonce, AccessToken string }

func (BrowserProof) String() string { return "oidccore: browser proof (redacted)" }

// GoString prevents generic diagnostic disclosure of nonce/access tokens.
func (p BrowserProof) GoString() string { return p.String() }

// MarshalJSON prevents implicit serialization of nonce/access tokens.
func (BrowserProof) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

// Facts is a detached explicit proof projection. Nonce/hash are credential data;
// generic formatting/JSON is redacted rather than a diagnostic disclosure path.
type Facts struct {
	Issuer, Subject                                     string
	Audiences                                           []string
	IssuedAt, ExpiresAt, NotBefore                      time.Time
	Nonce, AccessTokenHash, Algorithm, VerifierIdentity string
}

func (Facts) String() string { return "oidccore: facts (redacted)" }

// GoString prevents generic diagnostic disclosure of proof facts.
func (f Facts) GoString() string { return f.String() }

// MarshalJSON prevents implicit serialization of proof facts.
func (Facts) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

// VerifiedToken has no public constructor; zero values cannot evaluate policy.
// No raw token, mutable caller map or authorized Principal is retained.
type VerifiedToken struct {
	facts             Facts
	claims            map[string]any
	verified, browser bool
}

func (*VerifiedToken) String() string { return "oidccore: verified token (redacted)" }

// GoString prevents generic diagnostic disclosure of verified claims.
func (t *VerifiedToken) GoString() string { return t.String() }

// MarshalJSON prevents implicit serialization of verified claims.
func (*VerifiedToken) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

// Facts returns detached typed proof data for explicit trusted consumers.
func (t *VerifiedToken) Facts() Facts {
	if t == nil {
		return Facts{}
	}
	f := t.facts
	f.Audiences = append([]string(nil), f.Audiences...)
	return f
}

// Claims deliberately exposes a detached copy for trusted enrollment policy.
func (t *VerifiedToken) Claims() map[string]any {
	if t == nil || !t.verified {
		return nil
	}
	return cloneJSON(t.claims).(map[string]any)
}

// ValidAt rechecks actual supplied time, with no clock reads or expiry leeway.
func (t *VerifiedToken) ValidAt(now time.Time) error {
	if t == nil || !t.verified || now.IsZero() || now.Unix() < 0 || now.Year() > 9999 || t.facts.IssuedAt.After(now) || !now.Before(t.facts.ExpiresAt) || !t.facts.NotBefore.IsZero() && now.Before(t.facts.NotBefore) {
		return ErrTime
	}
	return nil
}
