package memberidentity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const enrollmentPurpose = "kudurru/member-enrollment-proof/v1\x00"

// KeyFingerprint identifies the original public signing key independently of
// certificate serial, lifetime or member UUID. Enrollment preserves revoked keys
// so reissuing a certificate cannot resurrect a retired identity.
func (t Trust) KeyFingerprint() string {
	if t.root == nil {
		return ""
	}
	sum := sha256.Sum256(t.root.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ValidAt checks the original CA's certificate lifetime without any lookup.
// A valid certificate is not current enrollment or domain authorization.
func (t Trust) ValidAt(now time.Time) bool {
	return t.root != nil && !now.Before(t.root.NotBefore) && now.Before(t.root.NotAfter)
}

// ExpiresAt returns the original CA's absolute expiration for transport bounds.
// It grants no trust, enrollment or renewal; invalid trust returns zero.
func (t Trust) ExpiresAt() time.Time {
	if t.root == nil {
		return time.Time{}
	}
	return t.root.NotAfter
}

// SignEnrollment proves possession of this node's original private CA key for
// a bounded enrollment statement. Purpose separation prevents using this method
// to sign a certificate, TLS handshake or another protocol's message.
func (i *Identity) SignEnrollment(statement []byte) ([]byte, error) {
	if i == nil || len(statement) == 0 || len(statement) > 8192 || !i.trust.ValidAt(time.Now()) {
		return nil, ErrIdentity
	}
	return ed25519.Sign(i.key, append([]byte(enrollmentPurpose), statement...)), nil
}

// VerifyEnrollment checks only possession and purpose. The enrollment owner
// must separately check original issuer, token, target, expiry and current policy.
func (t Trust) VerifyEnrollment(statement, signature []byte) error {
	if t.root == nil || len(statement) == 0 || len(statement) > 8192 || len(signature) != ed25519.SignatureSize {
		return ErrIdentity
	}
	if !ed25519.Verify(t.root.PublicKey.(ed25519.PublicKey), append([]byte(enrollmentPurpose), statement...), signature) {
		return ErrIdentity
	}
	return nil
}
