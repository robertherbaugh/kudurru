package oidccore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Verify verifies an ID token offline and rechecks each supplied time. Audience
// and claim policy remain a separate required Evaluate step. No verdict is cached.
func (v *Verifier) Verify(ctx context.Context, raw string, now time.Time) (*VerifiedToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == nil || v.identity == "" {
		return nil, ErrInvalid
	}
	if len(raw) > MaxTokenBytes {
		return nil, ErrBounds
	}
	parts := strings.SplitN(raw, ".", 4)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrInvalid
	}
	header, err := decodeSegment(parts[0], MaxHeaderBytes)
	if err != nil {
		return nil, err
	}
	h, err := parseObject(header, MaxHeaderBytes)
	if err != nil {
		return nil, err
	}
	for k := range h {
		if k != "alg" && k != "kid" && k != "typ" {
			return nil, ErrInvalid
		}
	}
	alg, ok := h["alg"].(string)
	if !ok || !slices.Contains(v.algorithms, jose.SignatureAlgorithm(alg)) {
		return nil, ErrSignature
	}
	kid, err := optionalString(h, "kid", 256)
	if err != nil {
		return nil, ErrInvalid
	}
	typ, err := optionalString(h, "typ", 128)
	if err != nil || strings.EqualFold(typ, "at+jwt") || strings.EqualFold(typ, "application/at+jwt") {
		return nil, ErrClaims
	}
	payload, err := decodeSegment(parts[1], MaxClaimsBytes)
	if err != nil {
		return nil, err
	}
	if _, err = decodeSegment(parts[2], 1024); err != nil {
		return nil, err
	}
	claims, err := parseObject(payload, MaxClaimsBytes)
	if err != nil {
		return nil, err
	}
	signed, err := jose.ParseSignedCompact(raw, v.algorithms)
	if err != nil {
		return nil, ErrSignature
	}
	matched := false
	for _, key := range v.keys {
		if kid != "" && kid != key.kid || kid == "" && len(v.keys) != 1 || key.algorithm != "" && key.algorithm != alg || !keyAlgorithm(key.key, alg) {
			continue
		}
		verified, err := signed.Verify(key.key)
		if err == nil && bytes.Equal(verified, payload) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, ErrSignature
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	facts, err := tokenFacts(claims)
	if err != nil {
		return nil, err
	}
	if facts.Issuer != v.issuer {
		return nil, ErrClaims
	}
	facts.Algorithm, facts.VerifierIdentity = alg, v.identity
	t := &VerifiedToken{facts: facts, claims: claims, verified: true}
	if err = t.ValidAt(now); err != nil {
		return nil, err
	}
	return t, nil
}

// VerifyBrowser additionally binds the verified proof to expected preauth nonce
// and at_hash. It cannot exchange tokens, validate state or enroll a session.
func (v *Verifier) VerifyBrowser(ctx context.Context, raw string, proof BrowserProof, now time.Time) (*VerifiedToken, error) {
	if !validText(proof.Nonce, 512) || len(proof.AccessToken) > MaxTokenBytes {
		return nil, ErrBrowser
	}
	t, err := v.Verify(ctx, raw, now)
	if err != nil {
		return nil, err
	}
	if !constantEqual(t.facts.Nonce, proof.Nonce) {
		return nil, ErrBrowser
	}
	if t.facts.AccessTokenHash != "" {
		if proof.AccessToken == "" {
			return nil, ErrBrowser
		}
		var sum []byte
		switch t.facts.Algorithm {
		case "RS256", "PS256", "ES256":
			h := sha256.Sum256([]byte(proof.AccessToken))
			sum = h[:]
		case "RS384", "PS384", "ES384":
			h := sha512.Sum384([]byte(proof.AccessToken))
			sum = h[:]
		case "RS512", "PS512", "ES512", "EdDSA":
			h := sha512.Sum512([]byte(proof.AccessToken))
			sum = h[:]
		default:
			return nil, ErrBrowser
		}
		if !constantEqual(base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2]), t.facts.AccessTokenHash) {
			return nil, ErrBrowser
		}
	}
	t.browser = true
	return t, nil
}

func constantEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func tokenFacts(m map[string]any) (Facts, error) {
	f := Facts{}
	var err error
	f.Issuer, err = optionalString(m, "iss", MaxIssuerBytes)
	if err != nil || f.Issuer == "" {
		return Facts{}, ErrClaims
	}
	f.Subject, err = optionalString(m, "sub", MaxSubjectBytes)
	if err != nil || f.Subject == "" {
		return Facts{}, ErrClaims
	}
	f.IssuedAt, err = numericDate(m["iat"])
	if err != nil {
		return Facts{}, err
	}
	f.ExpiresAt, err = numericDate(m["exp"])
	if err != nil || !f.ExpiresAt.After(f.IssuedAt) {
		return Facts{}, ErrTime
	}
	if value, exists := m["nbf"]; exists {
		f.NotBefore, err = numericDate(value)
		if err != nil || !f.NotBefore.Before(f.ExpiresAt) {
			return Facts{}, ErrTime
		}
	}
	switch a := m["aud"].(type) {
	case string:
		f.Audiences = []string{a}
	case []any:
		if len(a) > MaxAudiences {
			return Facts{}, ErrBounds
		}
		for _, v := range a {
			s, ok := v.(string)
			if !ok {
				return Facts{}, ErrClaims
			}
			f.Audiences = append(f.Audiences, s)
		}
	default:
		return Facts{}, ErrClaims
	}
	if len(f.Audiences) == 0 {
		return Facts{}, ErrClaims
	}
	seen := map[string]bool{}
	for _, a := range f.Audiences {
		if !validText(a, 256) || seen[a] {
			return Facts{}, ErrClaims
		}
		seen[a] = true
	}
	if _, err = optionalString(m, "azp", 256); err != nil {
		return Facts{}, ErrClaims
	}
	if flag, exists := m["email_verified"]; exists {
		switch value := flag.(type) {
		case bool:
		case string:
			if value != "true" && value != "false" {
				return Facts{}, ErrClaims
			}
		default:
			return Facts{}, ErrClaims
		}
	}
	f.Nonce, err = optionalString(m, "nonce", 512)
	if err != nil {
		return Facts{}, ErrClaims
	}
	f.AccessTokenHash, err = optionalString(m, "at_hash", 128)
	if err != nil {
		return Facts{}, ErrClaims
	}
	if f.AccessTokenHash != "" {
		b, err := decodeSegment(f.AccessTokenHash, 32)
		if err != nil || len(b) != 16 && len(b) != 24 && len(b) != 32 {
			return Facts{}, ErrClaims
		}
	}
	return f, nil
}

// The v1 profile accepts canonical whole Unix seconds through year 9999. It
// never rounds fractions or exponent notation across a persistent floor.
func numericDate(v any) (time.Time, error) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, ErrTime
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || i < 0 || i > 253402300799 || strconv.FormatInt(i, 10) != string(n) {
		return time.Time{}, ErrTime
	}
	return time.Unix(i, 0).UTC(), nil
}
