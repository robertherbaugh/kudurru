package oidc

import (
	"errors"
	"net/url"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// IdentityKey is the §8.1 identity of an OIDC user: (provider, iss, sub).
// Email is never part of it: addresses are reassigned, sub is not.
type IdentityKey struct {
	Provider string
	Issuer   string
	Subject  string
}

const identityKeyPrefix = "oidc:"

// String renders the key as Principal.ID:
// "oidc:<provider>:<issuer>:<subject>", each part query-escaped so the
// colon separators are unambiguous.
func (k IdentityKey) String() string {
	return identityKeyPrefix + url.QueryEscape(k.Provider) + ":" + url.QueryEscape(k.Issuer) + ":" + url.QueryEscape(k.Subject)
}

// ParseIdentityKey parses the output of IdentityKey.String.
func ParseIdentityKey(s string) (IdentityKey, error) {
	rest, ok := strings.CutPrefix(s, identityKeyPrefix)
	if !ok {
		return IdentityKey{}, errors.New("oidc: identity key lacks the oidc: prefix")
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return IdentityKey{}, errors.New("oidc: identity key must have three parts")
	}
	var k IdentityKey
	for i, dst := range []*string{&k.Provider, &k.Issuer, &k.Subject} {
		v, err := url.QueryUnescape(parts[i])
		if err != nil || v == "" {
			return IdentityKey{}, errors.New("oidc: malformed identity key")
		}
		*dst = v
	}
	return k, nil
}

// Identity is the result of the claim pipeline for one login.
type Identity struct {
	Key      IdentityKey
	Username string // prefixed
	// UsernameClaim is the claim the username came from, for logs.
	UsernameClaim string
	DisplayName   string   // informational
	Email         string   // set only when email_verified is true
	Groups        []string // prefixed, sorted, de-duplicated
}

// AuthMethodOIDC is Principal.AuthMethod during the callback.
const AuthMethodOIDC = "oidc"

// Principal converts the identity to the principal used for binding
// resolution. Groups are present here and dropped when the session is
// sealed (D48).
func (id Identity) Principal() auth.Principal {
	return auth.Principal{
		Kind:       auth.KindUser,
		ID:         id.Key.String(),
		Name:       id.Username,
		Groups:     id.Groups,
		Provider:   id.Key.Provider,
		AuthMethod: AuthMethodOIDC,
	}
}
