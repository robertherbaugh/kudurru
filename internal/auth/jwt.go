package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// MaxBearerBytes bounds a bearer credential. ID tokens of users in many
// groups are large (an 11 KB dex token for 200 groups), so the cap is
// generous, but it keeps a hostile header from costing a JWKS round trip.
const MaxBearerBytes = 64 << 10

// JWTVerifier verifies bearer JWTs from the issuers it owns: an OIDC
// provider (ID tokens for the CLI, §8.3) or the Kubernetes cluster
// (projected ServiceAccount tokens, §5.5).
type JWTVerifier interface {
	// AcceptsIssuer reports whether tokens with this (unverified) iss are
	// this verifier's to judge.
	AcceptsIssuer(ctx context.Context, iss string) bool
	// VerifyJWT verifies raw and returns its principal; any error is a 401.
	VerifyJWT(ctx context.Context, raw string) (Principal, error)
}

// BearerJWT is the JWT step of the chain. It reads the unverified iss of a
// bearer JWT and hands the token to the first verifier that owns that
// issuer; Fallback (the Kubernetes TokenReview path, used when the
// cluster's issuer could not be discovered) judges tokens nobody owns.
// Bearer values that are not JWTs (kdr_ tokens, the internal token) are
// left to the other steps.
type BearerJWT struct {
	Verifiers []JWTVerifier
	// Fallback may be nil: unowned issuers are then rejected.
	Fallback JWTVerifier
}

// ErrUnknownIssuer is the cause of a 401 for a JWT whose issuer no verifier
// owns.
var ErrUnknownIssuer = errors.New("auth: no verifier for this token issuer")

// Authenticate implements Authenticator.
func (b BearerJWT) Authenticate(_ http.ResponseWriter, r *http.Request) (Principal, *http.Request, error) {
	raw := BearerToken(r)
	if raw == "" || strings.HasPrefix(raw, "kdr_") {
		return Principal{}, nil, ErrNoCredentials
	}
	iss, ok := UnverifiedIssuer(raw)
	if !ok {
		return Principal{}, nil, ErrNoCredentials
	}
	if len(raw) > MaxBearerBytes {
		return Principal{}, nil, Unauthenticated(errors.New("bearer token too large"))
	}
	ctx := r.Context()
	for _, v := range b.Verifiers {
		if v.AcceptsIssuer(ctx, iss) {
			return verified(v, r, raw)
		}
	}
	if b.Fallback != nil {
		return verified(b.Fallback, r, raw)
	}
	return Principal{}, nil, Unauthenticated(ErrUnknownIssuer)
}

func verified(v JWTVerifier, r *http.Request, raw string) (Principal, *http.Request, error) {
	p, err := v.VerifyJWT(r.Context(), raw)
	if err != nil {
		return Principal{}, nil, AsError(err)
	}
	return p, r, nil
}

// UnverifiedIssuer returns the iss claim of a compact JWS without verifying
// it, for routing only. ok is false when raw is not a three-part JWT with
// a JSON payload carrying a string iss.
func UnverifiedIssuer(raw string) (iss string, ok bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", false
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Iss == "" {
		return "", false
	}
	return c.Iss, true
}
