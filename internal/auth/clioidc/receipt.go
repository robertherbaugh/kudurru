// Package clioidc holds bounded public-client login correlation. A receipt is
// reusable source evidence, never a credential, enrollment or one-time grant.
package clioidc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Lifetime bounds correlation without extending an OP grant or proof.
const Lifetime = 10 * time.Minute

// MaxReceiptBytes bounds receipt allocation before decryption.
const MaxReceiptBytes = 8192

// ErrInvalid never contains credential or upstream diagnostic text.
var ErrInvalid = errors.New("CLI login proof rejected")

// Source identifies the exact admitted provider capability, without secrets.
type Source struct {
	Provider, UID, Origin, Spec, Policy, Resource, Issuer, ClientID string
}

// PrepareRequest contains no provider endpoint or client identity override.
type PrepareRequest struct {
	Mode        string `json:"mode"`
	Nonce       string `json:"nonce,omitempty"`
	RedirectURI string `json:"redirectURI,omitempty"`
}

// Provider describes advertised public-client capability, not tested client registration.
type Provider struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Browser     bool   `json:"browser"`
	Device      bool   `json:"device"`
}

// Preparation returns public metadata and an opaque, non-authoritative receipt.
type Preparation struct {
	Provider         string    `json:"provider"`
	DisplayName      string    `json:"displayName"`
	Issuer           string    `json:"issuer"`
	ClientID         string    `json:"clientID"`
	AuthorizationURL string    `json:"authorizationURL"`
	TokenURL         string    `json:"tokenURL"`
	DeviceURL        string    `json:"deviceURL,omitempty"`
	Scopes           []string  `json:"scopes"`
	Receipt          string    `json:"receipt"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// VerifyRequest uses Authorization for the raw ID token. AccessToken is transient at_hash proof.
type VerifyRequest struct {
	Receipt     string `json:"receipt"`
	AccessToken string `json:"accessToken,omitempty"`
}

func (VerifyRequest) String() string { return "private CLI token response" }

// GoString redacts transient token-response proof.
func (p VerifyRequest) GoString() string { return p.String() }

type payload struct {
	Version   int            `json:"version"`
	Source    Source         `json:"source"`
	Request   PrepareRequest `json:"request"`
	IssuedAt  time.Time      `json:"issuedAt"`
	ExpiresAt time.Time      `json:"expiresAt"`
}

// Receipt cannot be constructed by a caller-authored payload.
type Receipt struct{ value payload }

func (Receipt) String() string { return "private non-authoritative CLI receipt" }

// GoString redacts correlation evidence.
func (r Receipt) GoString() string { return r.String() }

// MarshalJSON prevents incidental receipt disclosure.
func (Receipt) MarshalJSON() ([]byte, error) { return []byte(`"private receipt"`), nil }

// Source returns detached provider correlation.
func (r Receipt) Source() Source { return r.value.Source }

// Request returns the original flow parameters.
func (r Receipt) Request() PrepareRequest { return r.value.Request }

// ExpiresAt returns the original absolute deadline.
func (r Receipt) ExpiresAt() time.Time { return r.value.ExpiresAt }

// Codec uses an independent purpose, never browser preauth or session cookies.
type Codec struct{ sealer *session.Sealer }

// NewCodec captures purpose-separated current and previous cluster keys.
func NewCodec(current, previous []byte) (*Codec, error) {
	s, e := session.NewSealer("oidc-cli-source/v1", current, previous)
	if e != nil {
		return nil, ErrInvalid
	}
	return &Codec{s}, nil
}

// NewReceipt correlates a public source and original absolute deadline.
func NewReceipt(source Source, request PrepareRequest, now time.Time) (Receipt, error) {
	r := Receipt{payload{1, source, request, now.UTC(), now.UTC().Add(Lifetime)}}
	return r, r.ValidAt(now)
}
func validHash(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RandomProof accepts only canonical 256-bit base64url values.
func RandomProof(v string) bool {
	b, e := base64.RawURLEncoding.Strict().DecodeString(v)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == v
}

// ValidAt rechecks original absolute time; equality and regression refuse.
func (r Receipt) ValidAt(now time.Time) error {
	p, s := r.value, r.value.Source
	if p.Version != 1 || len(validation.IsDNS1123Label(s.Provider)) != 0 || s.UID == "" || len(s.UID) > 512 || (s.Origin != "config" && s.Origin != "object") || !validHash(s.Spec) || !validHash(s.Policy) || !validHash(s.Resource) || !HTTPS(s.Issuer) || len(s.ClientID) == 0 || len(s.ClientID) > 256 || p.IssuedAt.IsZero() || p.IssuedAt.Year() < 1970 || p.ExpiresAt.Year() > 9999 || !p.ExpiresAt.Equal(p.IssuedAt.Add(Lifetime)) || now.Before(p.IssuedAt) || !now.Before(p.ExpiresAt) {
		return ErrInvalid
	}
	if e := p.Request.Validate(); e != nil {
		return e
	}
	return nil
}

// Validate accepts only supported flow fields and exact loopback targets.
func (p PrepareRequest) Validate() error {
	if p.Mode == "device" {
		if p.Nonce != "" || p.RedirectURI != "" {
			return ErrInvalid
		}
		return nil
	}
	if p.Mode != "browser" || !RandomProof(p.Nonce) || (p.RedirectURI != "http://127.0.0.1:8000/callback" && p.RedirectURI != "http://127.0.0.1:18000/callback") {
		return ErrInvalid
	}
	return nil
}

// HTTPS requires an HTTPS endpoint without userinfo or fragments.
func HTTPS(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

// Seal encrypts validated source correlation.
func (c *Codec) Seal(r Receipt, now time.Time) (string, error) {
	if c == nil || c.sealer == nil || r.ValidAt(now) != nil {
		return "", ErrInvalid
	}
	b, e := json.Marshal(r.value)
	defer clear(b)
	if e != nil || session.SealedLen(len(b)) > MaxReceiptBytes {
		return "", ErrInvalid
	}
	return c.sealer.Seal(b), nil
}

// Open checks exact canonical correlation and original time bounds.
func (c *Codec) Open(raw string, now time.Time) (Receipt, error) {
	if c == nil || c.sealer == nil || len(raw) > MaxReceiptBytes {
		return Receipt{}, ErrInvalid
	}
	b, _, e := c.sealer.Open(raw)
	defer clear(b)
	if e != nil || len(b) > MaxReceiptBytes {
		return Receipt{}, ErrInvalid
	}
	var p payload
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return Receipt{}, ErrInvalid
	}
	canonical, _ := json.Marshal(p)
	defer clear(canonical)
	if !bytes.Equal(b, canonical) {
		return Receipt{}, ErrInvalid
	}
	r := Receipt{p}
	return r, r.ValidAt(now)
}
