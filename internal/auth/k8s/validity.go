package k8s

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"k8s.io/apimachinery/pkg/util/validation"
)

// MaxTokenLifetime bounds offline authority after ServiceAccount/pod deletion.
// It is a hard admission bound, not a replacement expiry for longer credentials.
// Project a kudurru-audience token with expirationSeconds: 600 and rotate it.
const MaxTokenLifetime = 10 * time.Minute

// Safe categories never include a credential, remote error body or claims.
var (
	errToken       = errors.New("k8s: projected credential rejected")
	errUnavailable = errors.New("k8s: credential verification unavailable")
)

type tokenBounds struct{ issuedAt, expiresAt, notBefore time.Time }

func (b tokenBounds) validAt(now time.Time) bool {
	return !now.IsZero() && !b.issuedAt.After(now) && now.Before(b.expiresAt) &&
		(b.notBefore.IsZero() || !now.Before(b.notBefore))
}

// projectedBounds is only admission of untrusted input. JWKS verification or an
// authoritative matching TokenReview remains mandatory, even on the first use.
func projectedBounds(raw string, now time.Time) (saClaims, tokenBounds, error) {
	iat, exp, nbf, err := oidccore.UnverifiedJWTTimeBounds(raw)
	bounds := tokenBounds{issuedAt: iat, expiresAt: exp, notBefore: nbf}
	if err != nil || exp.Sub(iat) > MaxTokenLifetime || !bounds.validAt(now) {
		return saClaims{}, tokenBounds{}, errToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.SplitN(raw, ".", 3)[1])
	var claims saClaims
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return saClaims{}, tokenBounds{}, errToken
	}
	ns, name, uid := claims.K8s.Namespace, claims.K8s.ServiceAccount.Name, claims.K8s.ServiceAccount.UID
	if len(validation.IsDNS1123Label(ns)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 ||
		uid == "" || len(uid) > 512 || strings.ContainsAny(uid, " \t\r\n") || claims.Sub != ServiceAccountPrefix+ns+":"+name {
		return saClaims{}, tokenBounds{}, errToken
	}
	return claims, bounds, nil
}
