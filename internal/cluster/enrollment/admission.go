package enrollment

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

// Admission retains one actual key-possession verification, original context,
// issuer, before/after state and absolute invitation/certificate lifetime. It is
// not a commit receipt; only the store's actual completion can claim durability.
type Admission struct {
	ctx                    context.Context
	issuer                 Issuer
	before, after          string
	state                  *State
	expires                time.Time
	member, originalIssuer memberidentity.Trust
	life                   *admissionLife
}

// Copies of the opaque handle share terminal state and the original high-water
// clock. Copying a public struct cannot manufacture another admission lifetime.
type admissionLife struct {
	last   atomic.Int64
	closed atomic.Bool
}

// Admit samples the actual clock under an original finite request, verifies
// possession once, and prepares the exact immutable successor for persistence.
func (s *State) Admit(ctx context.Context, proposal Proposal, issuer Issuer) (*Admission, error) {
	if ctx == nil {
		return nil, ErrRefused
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > MaxInvitationLifetime {
		return nil, ErrRefused
	}
	after, err := s.Accept(proposal, issuer, now)
	if err != nil {
		return nil, err
	}
	beforeDigest, err := s.Digest()
	if err != nil {
		return nil, err
	}
	afterDigest, err := after.Digest()
	if err != nil {
		return nil, err
	}
	a := &Admission{ctx: ctx, issuer: issuer, before: beforeDigest, after: afterDigest, state: after, member: proposal.member.Trust, life: &admissionLife{}}
	for _, r := range s.doc.Invitations {
		if r.ID == proposal.invitation.record.ID {
			a.expires = r.ExpiresAt
			break
		}
	}
	for _, m := range s.Members() {
		if m.ID == issuer.MemberID {
			a.originalIssuer = m.Trust
			break
		}
	}
	a.life.last.Store(now.UnixNano())
	if err := a.Check(beforeDigest, afterDigest, issuer); err != nil {
		return nil, err
	}
	return a, nil
}

// State is the exact prepared successor, not a published or committed owner.
func (a *Admission) State() *State {
	if a == nil {
		return nil
	}
	return a.state
}

// Check rechecks only retained memory, original context and actual clock. It
// performs no I/O, reacquisition, signature verification or locking. Every
// refusal is terminal for this admission; a later clock/source cannot rescue it.
// The caller must compare the actual transaction's before/after digests and
// actual original issuer, retaining dependency exclusion through store commit.
func (a *Admission) Check(before, after string, issuer Issuer) error {
	if a == nil || a.life == nil || a.life.closed.Load() {
		return ErrRefused
	}
	fail := func(err error) error { a.life.closed.Store(true); return err }
	if a.ctx == nil || a.state == nil || len(a.before) != 64 || len(a.after) != 64 || !validIssuer(a.issuer) || a.expires.IsZero() {
		return fail(ErrRefused)
	}
	if err := a.ctx.Err(); err != nil {
		return fail(err)
	}
	now := time.Now()
	if before != a.before || after != a.after || issuer != a.issuer || !now.Before(a.expires) || !a.member.ValidAt(now) || !a.originalIssuer.ValidAt(now) {
		return fail(ErrRefused)
	}
	deadline, finite := a.ctx.Deadline()
	if !finite || !now.Before(deadline) {
		return fail(context.DeadlineExceeded)
	}
	nanos := now.UnixNano()
	if !time.Unix(0, nanos).Equal(now) {
		return fail(ErrRefused)
	}
	for {
		previous := a.life.last.Load()
		if nanos < previous {
			return fail(ErrRefused)
		}
		if a.life.last.CompareAndSwap(previous, nanos) {
			break
		}
	}
	if err := a.ctx.Err(); err != nil {
		return fail(err)
	}
	if a.life.closed.Load() {
		return ErrRefused
	}
	return nil
}
