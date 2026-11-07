package oidccore

import "strings"

// Issuer exposes the immutable verifier authority for resource matching.
func (v *Verifier) Issuer() string {
	if v == nil {
		return ""
	}
	return v.issuer
}

// RoutingIssuer returns UNVERIFIED input for exact routing only, never identity.
func RoutingIssuer(raw string) (string, error) {
	if len(raw) > MaxTokenBytes {
		return "", ErrBounds
	}
	parts := strings.SplitN(raw, ".", 4)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", ErrInvalid
	}
	header, err := decodeSegment(parts[0], MaxHeaderBytes)
	if err != nil {
		return "", err
	}
	if _, err = parseObject(header, MaxHeaderBytes); err != nil {
		return "", err
	}
	if _, err = decodeSegment(parts[2], 1024); err != nil {
		return "", err
	}
	payload, err := decodeSegment(parts[1], MaxClaimsBytes)
	if err != nil {
		return "", err
	}
	claims, err := parseObject(payload, MaxClaimsBytes)
	if err != nil {
		return "", err
	}
	issuer, err := optionalString(claims, "iss", MaxIssuerBytes)
	if err != nil || issuer == "" {
		return "", ErrClaims
	}
	return issuer, nil
}
