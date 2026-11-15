package oidccore

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
)

type verificationKey struct {
	kid, algorithm, thumbprint string
	key                        any
}

// Verifier pins one issuer, asymmetric allowlist and immutable public key set.
// No caller key slices, network handles, clock closures or verdict cache survive.
type Verifier struct {
	issuer, identity string
	algorithms       []jose.SignatureAlgorithm
	keys             []verificationKey
}

func (*Verifier) String() string { return "oidccore: verifier" }

// GoString omits key documents from generic diagnostics.
func (v *Verifier) GoString() string { return v.String() }

// MarshalJSON omits key documents from generic diagnostics.
func (*Verifier) MarshalJSON() ([]byte, error) { return []byte(`"verifier"`), nil }

// Identity pins the issuer, allowlist and canonical public-key acceptance set.
func (v *Verifier) Identity() string {
	if v == nil {
		return ""
	}
	return v.identity
}

// NewVerifier constructs an offline resource. Empty algorithms defaults to RS256.
// The host supplies already-fetched JWKS; parsing never grants endpoint access.
func NewVerifier(issuer string, algorithms []string, jwks []byte) (*Verifier, error) {
	if !validURL(issuer, false) {
		return nil, ErrInvalid
	}
	if len(algorithms) > 10 || len(jwks) > MaxJWKSBytes {
		return nil, ErrBounds
	}
	if len(algorithms) == 0 {
		algorithms = []string{"RS256"}
	}
	allow := slices.Clone(algorithms)
	slices.Sort(allow)
	v := &Verifier{issuer: issuer}
	for i, a := range allow {
		if !allowedAlgorithm(a) || i > 0 && a == allow[i-1] {
			return nil, ErrInvalid
		}
		v.algorithms = append(v.algorithms, jose.SignatureAlgorithm(a))
	}
	m, err := parseObjectArrays(jwks, MaxJWKSBytes, MaxKeys)
	if err != nil {
		return nil, err
	}
	rows, ok := m["keys"].([]any)
	if !ok || len(m) != 1 || len(rows) == 0 {
		return nil, ErrInvalid
	}
	if len(rows) > MaxKeys {
		return nil, ErrBounds
	}
	seenKid, seenKey := map[string]bool{}, map[string]bool{}
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			return nil, ErrInvalid
		}
		k, err := parseKey(row, allow)
		if err != nil {
			return nil, err
		}
		if seenKid[k.kid] || seenKey[k.thumbprint] || len(rows) > 1 && k.kid == "" {
			return nil, ErrInvalid
		}
		seenKid[k.kid], seenKey[k.thumbprint] = true, true
		v.keys = append(v.keys, k)
	}
	slices.SortFunc(v.keys, func(a, b verificationKey) int { return strings.Compare(a.kid, b.kid) })
	identities := make([][3]string, len(v.keys))
	for i, k := range v.keys {
		identities[i] = [3]string{k.kid, k.algorithm, k.thumbprint}
	}
	v.identity = digest(struct {
		Profile, Issuer string
		Algorithms      []string
		Keys            [][3]string
	}{"oidccore/verifier/v1", issuer, allow, identities})
	return v, nil
}

func parseKey(m map[string]any, allow []string) (verificationKey, error) {
	for name := range m {
		switch name {
		case "kty", "kid", "alg", "use", "key_ops", "n", "e", "crv", "x", "y", "x5c", "x5t", "x5t#S256":
		default:
			return verificationKey{}, ErrInvalid
		}
	}
	kid, err := optionalString(m, "kid", 256)
	if err != nil {
		return verificationKey{}, err
	}
	alg, err := optionalString(m, "alg", 16)
	if err != nil || alg != "" && !slices.Contains(allow, alg) {
		return verificationKey{}, ErrInvalid
	}
	use, err := optionalString(m, "use", 8)
	if err != nil || use != "" && use != "sig" {
		return verificationKey{}, ErrInvalid
	}
	if ops, exists := m["key_ops"]; exists {
		values, ok := ops.([]any)
		if !ok || len(values) != 1 || values[0] != "verify" {
			return verificationKey{}, ErrInvalid
		}
	}
	kty, ok := m["kty"].(string)
	if !ok {
		return verificationKey{}, ErrInvalid
	}
	var components []string
	switch kty {
	case "RSA":
		components = []string{"n", "e"}
	case "EC":
		components = []string{"x", "y"}
	case "OKP":
		components = []string{"x"}
	default:
		return verificationKey{}, ErrInvalid
	}
	for _, name := range components {
		s, ok := m[name].(string)
		if !ok {
			return verificationKey{}, ErrInvalid
		}
		b, err := decodeSegment(s, 1024)
		if err != nil || len(b) == 0 || kty == "RSA" && b[0] == 0 {
			return verificationKey{}, ErrInvalid
		}
	}
	// Reject irrelevant public-key fields rather than allowing parsers to choose
	// different representations. Certificate metadata is bounded and never fetched.
	for _, name := range []string{"n", "e", "x", "y", "crv"} {
		_, exists := m[name]
		wanted := slices.Contains(components, name) || name == "crv" && kty != "RSA"
		if exists != wanted {
			return verificationKey{}, ErrInvalid
		}
	}
	if certs, exists := m["x5c"]; exists {
		list, ok := certs.([]any)
		if !ok || len(list) == 0 || len(list) > 8 {
			return verificationKey{}, ErrInvalid
		}
		for _, c := range list {
			if _, ok := c.(string); !ok {
				return verificationKey{}, ErrInvalid
			}
		}
	}
	for _, name := range []string{"x5t", "x5t#S256"} {
		if s, exists := m[name]; exists {
			text, ok := s.(string)
			if !ok {
				return verificationKey{}, ErrInvalid
			}
			b, err := decodeSegment(text, 32)
			if err != nil || name == "x5t" && len(b) != 20 || name == "x5t#S256" && len(b) != 32 {
				return verificationKey{}, ErrInvalid
			}
		}
	}
	raw, _ := json.Marshal(m)
	var key jose.JSONWebKey
	if json.Unmarshal(raw, &key) != nil || !key.IsPublic() || !key.Valid() {
		return verificationKey{}, ErrInvalid
	}
	compatible := false
	for _, a := range allow {
		if (alg == "" || a == alg) && keyAlgorithm(key.Key, a) {
			compatible = true
		}
	}
	if !compatible {
		return verificationKey{}, ErrInvalid
	}
	thumb, err := key.Thumbprint(crypto.SHA256)
	if err != nil {
		return verificationKey{}, ErrInvalid
	}
	return verificationKey{kid: kid, algorithm: alg, thumbprint: hex.EncodeToString(thumb), key: key.Key}, nil
}

func keyAlgorithm(key any, algorithm string) bool {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return k.N.BitLen() >= 2048 && k.N.BitLen() <= 8192 && k.N.Bit(0) == 1 && k.E >= 3 && k.E <= 1<<31-1 && k.E%2 == 1 && (strings.HasPrefix(algorithm, "RS") || strings.HasPrefix(algorithm, "PS"))
	case *ecdsa.PublicKey:
		if _, err := k.ECDH(); err != nil {
			return false
		}
		return algorithm == "ES256" && k.Curve.Params().Name == "P-256" || algorithm == "ES384" && k.Curve.Params().Name == "P-384" || algorithm == "ES512" && k.Curve.Params().Name == "P-521"
	case ed25519.PublicKey:
		return algorithm == "EdDSA" && len(k) == ed25519.PublicKeySize
	default:
		return false
	}
}

func allowedAlgorithm(s string) bool {
	switch s {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA":
		return true
	}
	return false
}

func digest(value any) string {
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func decodeSegment(s string, limit int) ([]byte, error) {
	if len(s) > base64.RawURLEncoding.EncodedLen(limit) {
		return nil, ErrBounds
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, ErrInvalid
	}
	return b, nil
}

func optionalString(m map[string]any, key string, limit int) (string, error) {
	v, exists := m[key]
	if !exists {
		return "", nil
	}
	s, ok := v.(string)
	if !ok || !validText(s, limit) {
		return "", ErrInvalid
	}
	return s, nil
}
