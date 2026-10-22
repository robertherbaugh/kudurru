package oidccore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Evidence size caps are checked before decryption or typed decoding.
const (
	MaxEvidencePlaintextBytes = 40 << 10
	MaxEvidenceSealedBytes    = 56 << 10
	evidencePurpose           = "oidc-enrollment-evidence"
)

// EvidenceKind separates expiring request facts from durable session evidence.
type EvidenceKind string

const (
	// RequestEvidence can never substitute for an enrolled session.
	RequestEvidence EvidenceKind = "request"
	// SessionEvidence requires a durable UID-bound enrollment.
	SessionEvidence EvidenceKind = "session"
)

// EvidenceBinding authenticates the exact provider and credential incarnation.
type EvidenceBinding struct {
	Kind        EvidenceKind `json:"kind"`
	Provider    string       `json:"provider"`
	ProviderUID string       `json:"providerUID"`
	Origin      string       `json:"origin"`
	Issuer      string       `json:"issuer"`
	Subject     string       `json:"subject"`
	IdentityUID string       `json:"identityUID"`
	RecordID    string       `json:"recordID"`
}

// EvidenceSource pins the actual issuance policy and verifier resource cut.
type EvidenceSource struct {
	SpecIdentity     string `json:"specIdentity"`
	PolicyIdentity   string `json:"policyIdentity"`
	ResourceIdentity string `json:"resourceIdentity"`
}

// EvidenceKeyring is an immutable purpose-separated current/previous key pair.
type EvidenceKeyring struct {
	sealer   *session.Sealer
	identity string
}

// EnrollmentEvidence has no public constructor or mutable claims projection.
type EnrollmentEvidence struct {
	binding    EvidenceBinding
	source     EvidenceSource
	token      *VerifiedToken
	mode       ProofMode
	verifiedAt time.Time
}

func (*EvidenceKeyring) String() string { return "private OIDC evidence keyring" }

// GoString prevents key disclosure.
func (k *EvidenceKeyring) GoString() string { return k.String() }

// MarshalJSON prevents accidental key persistence.
func (*EvidenceKeyring) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }
func (*EnrollmentEvidence) String() string            { return "private enrolled OIDC evidence" }

// GoString prevents claim disclosure.
func (e *EnrollmentEvidence) GoString() string { return e.String() }

// MarshalJSON prevents accidental claim persistence.
func (*EnrollmentEvidence) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

// Identity pins exact key resources without disclosing key bytes.
func (k *EvidenceKeyring) Identity() string {
	if k == nil {
		return ""
	}
	return k.identity
}

// NewEvidenceKeyring copies purpose-separated crypto resources without I/O.
func NewEvidenceKeyring(current, previous []byte) (*EvidenceKeyring, error) {
	sealer, err := session.NewSealer(evidencePurpose, current, previous)
	if err != nil {
		return nil, ErrInvalid
	}
	a, b := sha256.Sum256(current), sha256.Sum256(previous)
	h := sha256.Sum256(append(a[:], b[:]...))
	return &EvidenceKeyring{sealer: sealer, identity: hex.EncodeToString(h[:])}, nil
}

// Source returns detached issuance provenance, not current authority.
func (e *EnrollmentEvidence) Source() EvidenceSource {
	if e == nil {
		return EvidenceSource{}
	}
	return e.source
}

// Binding returns the authenticated owner tuple.
func (e *EnrollmentEvidence) Binding() EvidenceBinding {
	if e == nil {
		return EvidenceBinding{}
	}
	return e.binding
}

// Facts returns detached source facts; enrollment does not extend token validity.
func (e *EnrollmentEvidence) Facts() Facts {
	if e == nil {
		return Facts{}
	}
	return e.token.Facts()
}

// VerifiedAt is the actual source proof verification time.
func (e *EnrollmentEvidence) VerifiedAt() time.Time {
	if e == nil {
		return time.Time{}
	}
	return e.verifiedAt
}

var evidenceUID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var evidenceRecordID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var evidenceDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validEvidenceBinding(b EvidenceBinding) bool {
	if len(validation.IsDNS1123Label(b.Provider)) != 0 || !validText(b.ProviderUID, 512) || b.Origin != "object" && b.Origin != "config" || !validText(b.Issuer, MaxIssuerBytes) || !validText(b.Subject, MaxSubjectBytes) {
		return false
	}
	switch b.Kind {
	case RequestEvidence:
		return b.RecordID == "" && (b.IdentityUID == "" || evidenceUID.MatchString(b.IdentityUID))
	case SessionEvidence:
		return evidenceRecordID.MatchString(b.RecordID) && evidenceUID.MatchString(b.IdentityUID)
	default:
		return false
	}
}
func validEvidenceSource(s EvidenceSource) bool {
	return evidenceDigest.MatchString(s.SpecIdentity) && evidenceDigest.MatchString(s.PolicyIdentity) && evidenceDigest.MatchString(s.ResourceIdentity)
}

type evidencePayload struct {
	FormatVersion    int             `json:"formatVersion"`
	Binding          EvidenceBinding `json:"binding"`
	Source           EvidenceSource  `json:"source"`
	Mode             ProofMode       `json:"mode"`
	VerifiedAt       time.Time       `json:"verifiedAt"`
	Algorithm        string          `json:"algorithm"`
	VerifierIdentity string          `json:"verifierIdentity"`
	Claims           json.RawMessage `json:"claims"`
}

// SealVerified can enroll only opaque signature-verified facts valid at issuance.
func (k *EvidenceKeyring) SealVerified(binding EvidenceBinding, source EvidenceSource, token *VerifiedToken, mode ProofMode, verifiedAt time.Time) (string, error) {
	if k == nil || k.sealer == nil || !validEvidenceBinding(binding) || !validEvidenceSource(source) || token == nil || !token.verified || binding.Issuer != token.facts.Issuer || binding.Subject != token.facts.Subject || token.facts.VerifierIdentity == "" {
		return "", ErrInvalid
	}
	if err := token.ValidAt(verifiedAt); err != nil {
		return "", err
	}
	if mode != BearerIDToken && mode != BrowserIDToken || mode == BrowserIDToken && !token.browser {
		return "", ErrBrowser
	}
	claims, err := json.Marshal(token.claims)
	if err != nil || len(claims) > MaxClaimsBytes {
		return "", ErrBounds
	}
	payload := evidencePayload{1, binding, source, mode, verifiedAt.UTC(), token.facts.Algorithm, token.facts.VerifierIdentity, claims}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > MaxEvidencePlaintextBytes || session.SealedLen(len(raw)) > MaxEvidenceSealedBytes {
		return "", ErrBounds
	}
	if _, err = parseObject(raw, MaxEvidencePlaintextBytes); err != nil {
		return "", ErrBounds
	}
	return k.sealer.Seal(raw), nil
}
func exactFields(m map[string]any, names ...string) bool {
	if len(m) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := m[name]; !ok {
			return false
		}
	}
	return true
}

// Open creates opaque evidence only after exact binding and AEAD authentication.
func (k *EvidenceKeyring) Open(binding EvidenceBinding, sealed string) (*EnrollmentEvidence, error) {
	if k == nil || k.sealer == nil || !validEvidenceBinding(binding) || len(sealed) > MaxEvidenceSealedBytes {
		return nil, ErrInvalid
	}
	raw, _, err := k.sealer.Open(sealed)
	if err != nil {
		return nil, ErrInvalid
	}
	m, err := parseObject(raw, MaxEvidencePlaintextBytes)
	if err != nil || !exactFields(m, "formatVersion", "binding", "source", "mode", "verifiedAt", "algorithm", "verifierIdentity", "claims") {
		return nil, ErrInvalid
	}
	bm, ok := m["binding"].(map[string]any)
	if !ok || !exactFields(bm, "kind", "provider", "providerUID", "origin", "issuer", "subject", "identityUID", "recordID") {
		return nil, ErrInvalid
	}
	sm, ok := m["source"].(map[string]any)
	if !ok || !exactFields(sm, "specIdentity", "policyIdentity", "resourceIdentity") {
		return nil, ErrInvalid
	}
	var payload evidencePayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || payload.FormatVersion != 1 || payload.Binding != binding || !validEvidenceSource(payload.Source) || !evidenceDigest.MatchString(payload.VerifierIdentity) {
		return nil, ErrInvalid
	}
	claims, err := parseObject(payload.Claims, MaxClaimsBytes)
	if err != nil {
		return nil, err
	}
	facts, err := tokenFacts(claims)
	if err != nil || facts.Issuer != binding.Issuer || facts.Subject != binding.Subject {
		return nil, ErrClaims
	}
	if !allowedAlgorithm(payload.Algorithm) {
		return nil, ErrInvalid
	}
	facts.Algorithm, facts.VerifierIdentity = payload.Algorithm, payload.VerifierIdentity
	token := &VerifiedToken{facts: facts, claims: claims, verified: true, browser: payload.Mode == BrowserIDToken}
	if payload.Mode != BrowserIDToken && payload.Mode != BearerIDToken {
		return nil, ErrInvalid
	}
	if err = token.ValidAt(payload.VerifiedAt); err != nil {
		return nil, err
	}
	return &EnrollmentEvidence{binding, payload.Source, token, payload.Mode, payload.VerifiedAt}, nil
}

// EvaluateEnrollment checks current policy on frozen facts, not fresh upstream proof.
func (p *Policy) EvaluateEnrollment(ctx context.Context, e *EnrollmentEvidence, now time.Time) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if p == nil || e == nil || e.token == nil || e.binding.Kind != SessionEvidence || e.binding.Provider != p.id || e.binding.Issuer != p.spec.Issuer || now.IsZero() || now.Before(e.verifiedAt) || now.Year() > 9999 {
		return Identity{}, ErrPolicy
	}
	return p.evaluateClaims(ctx, e.token, e.mode)
}

// EvaluateRequest retains strict original proof expiry; it cannot evaluate a session.
func (p *Policy) EvaluateRequest(ctx context.Context, e *EnrollmentEvidence, now time.Time) (Identity, error) {
	if p == nil || e == nil || e.binding.Kind != RequestEvidence || e.binding.Provider != p.id {
		return Identity{}, ErrPolicy
	}
	return p.Evaluate(ctx, e.token, e.mode, now)
}
