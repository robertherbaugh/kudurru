package auth

import "time"

type credentialTime struct {
	authenticatedAt, issuedAt, expiresAt time.Time
	now                                  func() time.Time
}

// WithCredentialTimeBounds retains the verified ORIGINAL credential deadline
// for later request authorization, including after slow delegated authorization.
// It must only be called by a successful verifier. It conveys no authority to a
// current command, which still needs the original raw proof and final checks.
func WithCredentialTimeBounds(p Principal, authenticatedAt, issuedAt, expiresAt time.Time, clock func() time.Time) Principal {
	if clock == nil {
		clock = time.Now
	}
	p.credentialTime = &credentialTime{authenticatedAt: authenticatedAt, issuedAt: issuedAt, expiresAt: expiresAt, now: clock}
	return p
}

func (p Principal) credentialCurrent() bool {
	v := p.credentialTime
	if v == nil {
		// Kubernetes identities cannot be restored from a serialized principal
		// without their original verifier's private time evidence.
		return p.Kind != KindKubernetesServiceAccount
	}
	now := v.now()
	return !v.authenticatedAt.IsZero() && !v.issuedAt.IsZero() && v.expiresAt.After(v.issuedAt) &&
		!now.IsZero() && !now.Before(v.authenticatedAt) && !now.Before(v.issuedAt) && now.Before(v.expiresAt)
}
