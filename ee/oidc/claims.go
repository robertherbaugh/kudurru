package oidc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Claims is a verified ID token payload decoded with json.Number, so large
// numeric claims compare exactly.
type Claims map[string]any

// DecodeClaims decodes a raw JSON claim set.
func DecodeClaims(raw []byte) (Claims, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var c Claims
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	return c, nil
}

// usernameFallback is the §8.4 chain tried after the configured claim.
var usernameFallback = []string{"preferred_username", "upn", "email", "sub"}

// rolesClaim is read in addition to the groups claim (Entra ID app roles,
// Keycloak client roles mapped flat, Technitium's behaviour).
const rolesClaim = "roles"

const maxUsernameLen = 256

// Evaluate runs the claim pipeline for a verified token: claim validation
// rules, then the username and groups mappings. issuer and subject are the
// verified iss and sub; they form the identity key with the provider ID.
func (c *ProviderConfig) Evaluate(issuer, subject string, claims Claims) (Identity, *LoginError) {
	for _, rule := range c.ClaimValidationRules {
		if !claimEquals(claims[rule.Claim], rule.RequiredValue) {
			return Identity{}, loginErr(http.StatusForbidden, CodeClaimRule,
				fmt.Sprintf("your account does not satisfy this identity provider's requirements (claim %q)", rule.Claim), nil)
		}
	}
	verified := emailVerified(claims)
	id := Identity{
		Key:         IdentityKey{Provider: c.ID, Issuer: issuer, Subject: subject},
		DisplayName: stringClaim(claims, c.ClaimMappings.DisplayName.Claim),
	}
	if verified {
		id.Email = stringClaim(claims, c.ClaimMappings.Email.Claim)
	}

	username, from := c.username(claims, subject, verified)
	if username == "" {
		return Identity{}, loginErr(http.StatusForbidden, CodeClaims,
			"the identity provider sent no usable username claim (tried "+strings.Join(c.usernameCandidates(), ", ")+")", nil)
	}
	id.Username = c.ClaimMappings.Username.Prefix + username
	id.UsernameClaim = from

	if _, distributed := distributedClaimNames(claims)[c.ClaimMappings.Groups.Claim]; distributed && claims[c.ClaimMappings.Groups.Claim] == nil {
		return Identity{}, loginErr(http.StatusForbidden, CodeDistributedClaim,
			"the identity provider sent your groups as a distributed claim (group overage); kudurru does not fetch distributed claims, so configure the IdP to emit only the groups assigned to this application", nil)
	}
	id.Groups = c.groups(claims)
	return id, nil
}

func (c *ProviderConfig) usernameCandidates() []string {
	out := make([]string, 0, 5)
	if c.ClaimMappings.Username.Claim != "" {
		out = append(out, c.ClaimMappings.Username.Claim)
	}
	for _, n := range usernameFallback {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// username walks the candidates; an email claim counts only when verified,
// because an unverified address is an attacker-chosen string at many IdPs.
func (c *ProviderConfig) username(claims Claims, subject string, verified bool) (value, claim string) {
	for _, name := range c.usernameCandidates() {
		var v string
		switch name {
		case "sub":
			v = subject
		case c.ClaimMappings.Email.Claim, "email":
			if !verified {
				continue
			}
			v = stringClaim(claims, name)
		default:
			v = stringClaim(claims, name)
		}
		if validUsername(v) {
			return v, name
		}
	}
	return "", ""
}

func validUsername(v string) bool {
	return v != "" && len(v) <= maxUsernameLen &&
		!strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// groups reads the configured groups claim and the roles claim, each as a
// string or an array (non-string members are ignored), prefixes, sorts and
// de-duplicates.
func (c *ProviderConfig) groups(claims Claims) []string {
	names := []string{c.ClaimMappings.Groups.Claim}
	if names[0] != rolesClaim {
		names = append(names, rolesClaim)
	}
	seen := map[string]struct{}{}
	var out []string
	for _, n := range names {
		for _, g := range stringsClaim(claims[n]) {
			g = c.ClaimMappings.Groups.Prefix + g
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// stringsClaim accepts "a", ["a","b"] and ignores everything else (numbers,
// objects, nulls, empty strings). Headscale calls this FlexibleStringSlice.
func stringsClaim(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func stringClaim(claims Claims, name string) string {
	s, _ := claims[name].(string)
	return s
}

// emailVerified accepts true and "true" (some IdPs, e.g. Cognito, send a
// string). A missing claim is false: §8.1 trusts email only when verified.
func emailVerified(claims Claims) bool {
	switch v := claims["email_verified"].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// claimEquals compares a scalar claim with a required value by its JSON
// text: strings exactly, booleans as true/false, numbers as written.
func claimEquals(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want
	case bool:
		return strconv.FormatBool(t) == want
	case json.Number:
		return t.String() == want
	}
	return false
}

// distributedClaimNames returns the claim names listed in _claim_names
// (OIDC Core §5.6.2), which Entra ID uses for group overage.
func distributedClaimNames(claims Claims) map[string]any {
	m, _ := claims["_claim_names"].(map[string]any)
	return m
}

// checkAudience enforces OIDC Core §3.1.3.7 rules 3-5 beyond go-oidc's
// "aud contains client_id": with several audiences azp must be present,
// and when azp is present it must be our client.
func checkAudience(aud []string, claims Claims, clientID string) *LoginError {
	azp, hasAzp := claims["azp"].(string)
	if len(aud) > 1 && !hasAzp {
		return loginErr(http.StatusUnauthorized, CodeAudience, "the ID token has several audiences and no authorized party (azp)", nil)
	}
	if hasAzp && azp != clientID {
		return loginErr(http.StatusUnauthorized, CodeAudience, "the ID token was issued to a different client (azp)", nil)
	}
	return nil
}
