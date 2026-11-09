package security

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ForwardedSession is the public half of a session that an enrolled follower
// prepares before forwarding a local login: a fresh random ID and the hash of
// a CSRF secret that never leaves the follower. Neither grants anything alone.
type ForwardedSession struct {
	ID       string
	CSRFHash string
}

// NewForwardedEnrollment generates a fresh follower-side enrollment. The
// follower keeps the returned Enrollment privately and forwards only its
// ForwardedSession; after the primary commits, Seal works once the follower's
// own complete view contains the session.
func NewForwardedEnrollment() (Enrollment, ForwardedSession) {
	e := Enrollment{id: randomID(), csrf: randomID()}
	return e, ForwardedSession{ID: e.id, CSRFHash: servingauth.CSRFHash(e.csrf)}
}

func (f ForwardedSession) valid() bool {
	id, err := base64.RawURLEncoding.DecodeString(f.ID)
	if err != nil || len(id) != 32 || base64.RawURLEncoding.EncodeToString(id) != f.ID {
		return false
	}
	hash, err := hex.DecodeString(f.CSRFHash)
	return err == nil && len(hash) == 32 && hex.EncodeToString(hash) == f.CSRFHash
}

// IssueForwardedLocalWithAdmission is IssueLocalWithAdmission for a login that
// an enrolled follower forwarded: the same shared counters, password, factor,
// account and policy checks, but the session row uses the follower-prepared ID
// and CSRF hash. An existing ID refuses. The CSRF secret is never received.
func IssueForwardedLocalWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, ip string, prepared ForwardedSession, clock func() time.Time) (id string, refused error, commandErr error) {
	if !prepared.valid() {
		return "", nil, servingauth.ErrInvalid
	}
	refused, commandErr = withPasswordAdmission(ctx, tx, proof, ip, clock, func(proof servingauth.Credential, clock func() time.Time) error {
		now := clock().UTC()
		d, err := servingauth.ResolveSessionCurrent(ctx, tx, deps, proof, now)
		if err != nil {
			return err
		}
		_, err = issueLocalDecisionWith(ctx, tx, d, now, prepared.ID, prepared.CSRFHash)
		return err
	})
	if refused != nil || commandErr != nil {
		return "", refused, commandErr
	}
	return prepared.ID, nil, nil
}
