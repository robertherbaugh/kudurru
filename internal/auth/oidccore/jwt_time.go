package oidccore

import (
	"strings"
	"time"
)

// UnverifiedJWTTimeBounds reads canonical JWT times using the same bounded,
// duplicate-rejecting claim parser as Verify. These are UNTRUSTED bounds, never
// signature, audience, identity or authorization evidence. Callers must verify
// the original proof separately before accepting any credential.
func UnverifiedJWTTimeBounds(raw string) (issuedAt, expiresAt, notBefore time.Time, err error) {
	if len(raw) > MaxTokenBytes {
		return issuedAt, expiresAt, notBefore, ErrBounds
	}
	parts := strings.SplitN(raw, ".", 4)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return issuedAt, expiresAt, notBefore, ErrInvalid
	}
	payload, err := decodeSegment(parts[1], MaxClaimsBytes)
	if err != nil {
		return issuedAt, expiresAt, notBefore, err
	}
	claims, err := parseObject(payload, MaxClaimsBytes)
	if err != nil {
		return issuedAt, expiresAt, notBefore, err
	}
	facts, err := tokenFacts(claims)
	if err != nil {
		return issuedAt, expiresAt, notBefore, err
	}
	return facts.IssuedAt, facts.ExpiresAt, facts.NotBefore, nil
}
