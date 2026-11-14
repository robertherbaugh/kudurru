package replica

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// MaxCredentialBytes matches the bounded compact JWT verifier. Credentials are
// untrusted wire data, not serialized authentication or authorization evidence.
const MaxCredentialBytes = 64 << 10

// CredentialProof holds only original raw bearer/cookie/JWT bytes. It confers no
// permission or verified lifetime. The primary must verify it in the actual
// command transaction, including dedup, and retain verified bounds through
// response emission. Password, TOTP and Kubernetes impersonation are not supported.
// JSON serialization is expressly for the authenticated member transport;
// never put command JSON in audit records or diagnostic logs.
type CredentialProof struct {
	credential servingauth.Credential
}

type credentialWire struct {
	Kind       servingauth.CredentialKind `json:"kind"`
	Value      string                     `json:"value"`
	ProviderID string                     `json:"providerID,omitempty"`
}

// Reference returns an untrusted credential reference for a version-2 subject
// claim: the API token ID, or a purpose-separated digest of the exact cookie/JWT
// wire proof, including its kind and original provider selector. Cookie and JWT
// principals have no API TokenID. Neither this reference nor its equality proves
// identity or permission: compare it only alongside the successful CURRENT raw
// verifier's principal, provider and issuance time before effects or dedup.
func (p *CredentialProof) Reference() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if p.credential.Kind == servingauth.APIToken {
		id, ok := tokens.Parse(p.credential.Value)
		if !ok {
			return "", ErrProtocol
		}
		return id, nil
	}
	wire, err := p.MarshalJSON()
	if err != nil {
		return "", err
	}
	framed := append([]byte("kudurru.replica.credential-reference.v1\x00"), wire...)
	sum := sha256.Sum256(framed)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// NewCredentialProof captures raw bytes without claiming they authenticate.
func NewCredentialProof(raw servingauth.Credential) (*CredentialProof, error) {
	p := &CredentialProof{credential: raw}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *CredentialProof) validate() error {
	if p == nil {
		return ErrUnauthorized
	}
	raw := p.credential
	if len(raw.Value) == 0 || len(raw.Value) > MaxCredentialBytes || raw.Username != "" || raw.TOTPCode != "" || len(raw.ProviderID) > 256 || !utf8.ValidString(raw.ProviderID) {
		return ErrProtocol
	}
	for _, b := range []byte(raw.Value) {
		if b < '!' || b > '~' {
			return ErrProtocol
		}
	}
	switch raw.Kind {
	case servingauth.APIToken, servingauth.Cookie:
		if raw.ProviderID != "" {
			return ErrProtocol
		}
	case servingauth.OIDCJWT:
	default:
		return ErrProtocol
	}
	return nil
}

// Raw returns detached untrusted proof for the current credential verifier.
func (p *CredentialProof) Raw() (servingauth.Credential, error) {
	if err := p.validate(); err != nil {
		return servingauth.Credential{}, err
	}
	return p.credential, nil
}

// String redacts raw proof from ordinary diagnostics.
func (*CredentialProof) String() string { return "replica credential (private)" }

// GoString redacts raw proof from Go diagnostics.
func (p *CredentialProof) GoString() string { return p.String() }

// LogValue redacts raw proof from structured logging.
func (p *CredentialProof) LogValue() slog.Value { return slog.StringValue(p.String()) }

// MarshalJSON emits bounded raw proof only for the versioned member wire.
func (p *CredentialProof) MarshalJSON() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	raw := p.credential
	return json.Marshal(credentialWire{Kind: raw.Kind, Value: raw.Value, ProviderID: raw.ProviderID})
}

// UnmarshalJSON rejects extra/duplicate fields and never retains prior proof on
// failure. Authentication remains the current domain owner's responsibility.
func (p *CredentialProof) UnmarshalJSON(b []byte) error {
	if p == nil {
		return ErrProtocol
	}
	p.credential = servingauth.Credential{}
	// JSON may escape every raw byte; the enclosing command has its own bound.
	if len(b) > 6*MaxCredentialBytes+2048 {
		return ErrProtocol
	}
	var wire credentialWire
	if err := decodeStrict(b, &wire); err != nil {
		return ErrProtocol
	}
	next, err := NewCredentialProof(servingauth.Credential{Kind: wire.Kind, Value: wire.Value, ProviderID: wire.ProviderID})
	if err != nil {
		return err
	}
	p.credential = next.credential
	return nil
}

func (p *CredentialProof) matchesKind(kind string) bool {
	if p == nil {
		return false
	}
	switch p.credential.Kind {
	case servingauth.APIToken:
		return kind == "api-token"
	case servingauth.Cookie:
		return kind == "session"
	case servingauth.OIDCJWT:
		return kind == "oidc"
	}
	return false
}
