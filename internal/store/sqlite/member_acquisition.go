package sqlite

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// MemberAcquisition is a Store-issued scope for one outgoing follower exchange.
// Its exact local enrollment and destination remain excluded from replacement
// through actual credential writes, response acquisition and term observation.
// It grants no installation, command, publication or promotion authority.
type MemberAcquisition struct{ original *memberAcquisition }

type memberAcquisition struct {
	s         *Store
	ctx       context.Context
	authority replication.Authority
	local     memberidentity.Trust
	peer      enrollment.Member
	deadline  time.Time
	at        time.Time
	mu        sync.Mutex
	active    bool
	attempted bool
	verified  *memberidentity.PeerAdmission
}

type memberAcquisitionKey struct{}

// MemberID returns the immutable original local member UUID.
func (a *MemberAcquisition) MemberID() string {
	if a == nil || a.original == nil {
		return ""
	}
	return a.original.authority.MemberID
}

// Term returns the immutable originally captured authority term.
func (a *MemberAcquisition) Term() replication.Term {
	if a == nil || a.original == nil {
		return replication.Term{}
	}
	return a.original.authority.Term
}

// Peer returns the immutable originally enrolled destination and trust.
func (a *MemberAcquisition) Peer() enrollment.Member {
	if a == nil || a.original == nil {
		return enrollment.Member{}
	}
	return a.original.peer
}

// WithMemberAcquisition admits only an already enrolled nonwritable receiver
// and its exact retained private-identity trust. The original context must
// expire within 30 seconds BEFORE queueing. The callback must perform only one
// bounded transport exchange and pure decoding, honoring ctx and joining all
// work before returning. It must not call Store, acquire other owner gates or
// retain the scope; only ObserveMemberAuthority below may update this Store.
//
// Holding wmu through this bounded exchange prevents membership revocation or
// local promotion from racing an actual credential write to the old peer. No
// transaction is held during network I/O. Replica installation occurs later
// under its separately precaptured original anchor/source admission.
func (s *Store) WithMemberAcquisition(ctx context.Context, local memberidentity.Trust, fn func(context.Context, *MemberAcquisition) error) error {
	return s.withMemberAcquisition(ctx, local, nil, "", fn)
}

// WithMemberProbe captures an enrolled primary's original peer and authority
// for one bounded metadata exchange. The target is selected from the actual
// roster under wmu, never from caller-supplied trust or an address. The same
// no-reentry, cancellation and joined-I/O contract as WithMemberAcquisition
// applies. It supplies no image installation or domain mutation authority.
func (s *Store) WithMemberProbe(ctx context.Context, local memberidentity.Trust, target string, fn func(context.Context, *MemberAcquisition) error) error {
	if target == "" || target == local.MemberID() {
		return enrollment.ErrRefused
	}
	return s.withMemberAcquisition(ctx, local, nil, target, fn)
}

// original, when present, is the caller's already captured complete source.
// Its admission occurs under the same exclusion as membership and transport;
// waiting for wmu must never adopt a successor generation.
func (s *Store) withMemberAcquisition(ctx context.Context, local memberidentity.Trust, original *GenerationToken, probe string, fn func(context.Context, *MemberAcquisition) error) error {
	if ctx == nil || fn == nil || HasWriterToken(ctx) || ctx.Value(memberAcquisitionKey{}) != nil {
		return replication.ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return err
	}
	if err := s.lockWriterContext(ctx); err != nil {
		return err
	}
	defer s.wmu.Unlock()
	if original != nil && (original.source != s || original.DNSOnly() || !original.Matches(s.Authority())) {
		return replication.ErrCAS
	}
	a, owner, err := s.ReadMemberEnrollment(ctx)
	if err != nil {
		return err
	}
	admitted := (probe == "" && (a.Mode == replication.Follower || a.Mode == replication.Fenced) && a.Term.PrimaryID != a.MemberID) ||
		(probe != "" && a.Mode == replication.Primary && a.Writable())
	if owner.State() == nil || !admitted || a.Conflict != nil || a.Divergence != nil ||
		local.MemberID() != a.MemberID || !local.ValidAt(time.Now()) {
		return enrollment.ErrRefused
	}
	target := a.Term.PrimaryID
	if probe != "" {
		target = probe
	}
	var peer enrollment.Member
	found := false
	for _, member := range owner.State().Members() {
		if member.Revoked {
			continue
		}
		if member.ID == a.MemberID {
			found = member.Trust.Fingerprint() == local.Fingerprint()
		}
		if member.ID == target {
			peer = member
		}
	}
	if !found || peer.ID == "" || !peer.Trust.ValidAt(time.Now()) {
		return enrollment.ErrRefused
	}
	for _, expiry := range []time.Time{local.ExpiresAt(), peer.Trust.ExpiresAt()} {
		if expiry.Before(deadline) {
			deadline = expiry
		}
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	o := &memberAcquisition{s: s, ctx: ctx, authority: a, local: local, peer: peer, deadline: deadline, at: now, active: true}
	defer func() { o.mu.Lock(); o.active = false; o.mu.Unlock() }()
	if err := o.check(); err != nil {
		return err
	}
	scoped := context.WithValue(ctx, memberAcquisitionKey{}, o)
	result := fn(scoped, &MemberAcquisition{original: o})
	o.mu.Lock()
	defer o.mu.Unlock()
	if result == nil && o.verified == nil {
		return enrollment.ErrRefused
	}
	return errors.Join(result, o.check())
}

// check runs while the scope's serial observer guard (if exposed) and wmu are
// held. Original request/certificate expiry cannot be renewed by a new context.
func (o *memberAcquisition) check() error {
	if !o.active {
		return enrollment.ErrRefused
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	if now.UTC().Before(o.at.UTC()) || !now.Before(o.deadline) {
		return context.DeadlineExceeded
	}
	if !o.local.ValidAt(now) || !o.peer.Trust.ValidAt(now) || o.s.isClosed() || o.s.writeBlocked.Load() {
		return enrollment.ErrRefused
	}
	if !reflect.DeepEqual(o.authority, o.s.Authority()) {
		return replication.ErrCAS
	}
	if o.verified != nil {
		return o.verified.Check()
	}
	return nil
}

// ObserveAuthority deliberately refuses metadata without actual TLS evidence.
// It also lets this scope satisfy the legacy observer interface without ever
// falling back to unauthenticated term adoption.
func (*MemberAcquisition) ObserveAuthority(context.Context, replication.Term) (replication.Authority, error) {
	return replication.Authority{}, enrollment.ErrRefused
}

// ObserveMemberAuthority independently verifies the actual response connection
// against the originally enrolled destination before persisting a term. This is
// the only Store operation allowed inside the callback. A higher/conflicting
// term fences immediately, but retires this acquisition: no body can continue
// under the newly observed authority or issue success for the old source.
func (a *MemberAcquisition) ObserveMemberAuthority(ctx context.Context, term replication.Term, connection *tls.ConnectionState) (replication.Authority, error) {
	if a == nil || a.original == nil || ctx == nil {
		return replication.Authority{}, enrollment.ErrRefused
	}
	o := a.original
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Value(memberAcquisitionKey{}) != o {
		return replication.Authority{}, enrollment.ErrRefused
	}
	if err := ctx.Err(); err != nil {
		return replication.Authority{}, err
	}
	if err := o.check(); err != nil {
		return replication.Authority{}, err
	}
	if o.attempted {
		return replication.Authority{}, enrollment.ErrRefused
	}
	o.attempted = true
	if err := term.Validate(); err != nil {
		return replication.Authority{}, err
	}
	peer, err := o.peer.Trust.CapturePeer(o.ctx, connection)
	if err != nil {
		return replication.Authority{}, err
	}
	observed, err := o.s.observeAuthorityLocked(ctx, term)
	err = errors.Join(err, o.check(), peer.Check())
	if err == nil {
		o.verified = peer
	}
	return observed, err
}
