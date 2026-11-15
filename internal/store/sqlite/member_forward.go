package sqlite

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// MemberForwardRequest retains one original public request's complete source,
// finite lifetime and local private-identity trust. Capture before acquiring the
// body. It conveys no user permission or primary completion. Copies share one
// consumption; a refused or canceled attempt cannot be revived or retargeted.
type MemberForwardRequest struct{ original *memberForwardRequest }

type memberForwardRequest struct {
	s           *Store
	ctx         context.Context
	token       GenerationToken
	local       memberidentity.Trust
	deadline    time.Time
	used        atomic.Bool
	acquisition atomic.Pointer[memberAcquisition]
}

type memberForwardKey struct{}

// CaptureMemberForward retains the supplied token without reading or replacing
// its source. The actual acquisition rechecks it after the writer-lock queue.
func (s *Store) CaptureMemberForward(ctx context.Context, token GenerationToken, local memberidentity.Trust) (*MemberForwardRequest, error) {
	if ctx == nil || HasWriterToken(ctx) || token.source != s || token.DNSOnly() ||
		token.authority.Mode != replication.Follower || local.MemberID() != token.authority.MemberID {
		return nil, replication.ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second || !local.ValidAt(now) {
		return nil, replication.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, blocked := s.ServingAuthority()
	if blocked || !token.Matches(a) {
		return nil, replication.ErrCAS
	}
	return &MemberForwardRequest{original: &memberForwardRequest{s: s, ctx: ctx, token: token, local: local, deadline: deadline}}, nil
}

// OriginalTerm is the immutable original destination term, never a new capture.
func (r *MemberForwardRequest) OriginalTerm() replication.Term {
	if r == nil || r.original == nil {
		return replication.Term{}
	}
	return r.original.token.authority.Term
}

// Deadline is the original request bound, established before body acquisition.
func (r *MemberForwardRequest) Deadline() time.Time {
	if r == nil || r.original == nil {
		return time.Time{}
	}
	return r.original.deadline
}

// WithMemberForward retains original source/membership exclusion through one
// transport exchange and synchronous response emission. Root must already hold
// publication -> provider -> host/dependency exclusions and retain raw user
// proof/credential expiry. fn must obey WithMemberAcquisition's no-reentry and
// bounded transport contract. It cannot wait for follower publication or install
// an image; those operations would require this same Store lock.
func (s *Store) WithMemberForward(ctx context.Context, local memberidentity.Trust, request *MemberForwardRequest, fn func(context.Context, *MemberAcquisition) error) error {
	if ctx == nil || request == nil || request.original == nil || fn == nil {
		return replication.ErrInvalid
	}
	o := request.original
	if o.s != s || local.Fingerprint() != o.local.Fingerprint() || !o.used.CompareAndSwap(false, true) {
		return replication.ErrInvalid
	}
	ctx, cancel := context.WithDeadline(ctx, o.deadline)
	done := make(chan struct{})
	stop := context.AfterFunc(o.ctx, func() { cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		cancel()
	}()
	if err := o.ctx.Err(); err != nil {
		return err
	}
	return s.withMemberAcquisition(ctx, o.local, &o.token, "", func(scope context.Context, acquisition *MemberAcquisition) error {
		o.acquisition.Store(acquisition.original)
		defer o.acquisition.Store(nil)
		scope = context.WithValue(scope, memberForwardKey{}, o)
		if err := request.Check(scope); err != nil {
			return err
		}
		if err := fn(scope, acquisition); err != nil {
			return err
		}
		return request.Check(scope)
	})
}

// Check authenticates only this active synchronous forwarding scope. Plain,
// foreign, escaped, canceled and deadline-extended contexts refuse. It grants
// neither user permission nor proof that a remote command committed.
func (r *MemberForwardRequest) Check(ctx context.Context) error {
	if r == nil || r.original == nil || ctx == nil {
		return replication.ErrInvalid
	}
	o := r.original
	active := o.acquisition.Load()
	if ctx.Value(memberForwardKey{}) != o || active == nil {
		return replication.ErrInvalid
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, finite := ctx.Deadline()
	if !finite || deadline.After(o.deadline) || !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	if deadline.After(active.deadline) {
		return context.DeadlineExceeded
	}
	if !o.token.Matches(o.s.Authority()) {
		return replication.ErrCAS
	}
	return active.check()
}
