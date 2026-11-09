package enrollment

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

// JoinObservation can only inspect an original invitation's historical
// acceptance. It has no proposal, encoding or invitation conversion method.
// Expiry is historical evidence, never renewed acceptance authority. Acquisition
// still requires current member TLS and the receiver's exact durable intent.
type JoinObservation struct{ original Invitation }

// ParseJoinObservation retains canonical invitation bytes, including original
// expiry. It permits an expired invitation but never an expired issuer CA.
func ParseJoinObservation(encoded string) (JoinObservation, error) {
	i, err := parseInvitation(encoded, true)
	if err != nil {
		return JoinObservation{}, err
	}
	return JoinObservation{original: i}, nil
}

// JoinFingerprint identifies the exact canonical invitation without persisting
// its bearer secret. Purpose separation prevents use as an acceptance hash.
func (i Invitation) JoinFingerprint() (string, error) {
	wire, err := i.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("kudurru/local-join-intent/v1\x00" + wire))
	return hex.EncodeToString(sum[:]), nil
}

// JoinFingerprint retains the original invitation identity, not a new ticket.
func (o JoinObservation) JoinFingerprint() (string, error) { return o.original.JoinFingerprint() }

// ClusterID returns the original invitation's cluster.
func (o JoinObservation) ClusterID() string { return o.original.ClusterID() }

// Target returns the original independently confirmed target.
func (o JoinObservation) Target() (string, string) { return o.original.Target() }

// IssuingPeer retains the original term, address and CA; no discovery is allowed.
func (o JoinObservation) IssuingPeer() (Issuer, string, memberidentity.Trust) {
	return o.original.IssuingPeer()
}

// CheckLifetime checks current certificate validity and the original issue
// bound. Invitation expiry is not a credential for this observation operation.
func (o JoinObservation) CheckLifetime(local memberidentity.Trust) error {
	i, now := o.original, time.Now()
	if len(i.secret) != 32 || now.Before(i.record.IssuedAt) || !i.record.IssuedAt.Before(i.record.ExpiresAt) || !i.issuerTrust.ValidAt(now) || !local.ValidAt(now) || local.MemberID() == i.record.Issuer.MemberID {
		return ErrRefused
	}
	return nil
}

// CheckAccepted requires the original consumed ticket and exact current member
// and primary CAs in the complete image. Missing or pruned provenance refuses.
func (o JoinObservation) CheckAccepted(s *State, local memberidentity.Trust) error {
	if err := o.CheckLifetime(local); err != nil {
		return err
	}
	return o.original.checkAccepted(s, local)
}
