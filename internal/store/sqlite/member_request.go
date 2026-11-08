package sqlite

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrMemberCompletionUnknown reports an unconfirmed peer command completion.
// Neither a successful proposal nor a driver error proves rollback or commit.
var ErrMemberCompletionUnknown = errors.New("store/sqlite: member command completion unknown")

// MemberRequest is an original peer request, captured before body acquisition.
// It permits one image read OR one command, followed by at most one output scope.
// Copies share consumption and terminal state. It supplies no domain permission.
type MemberRequest struct{ original *memberRequest }

// OriginalTerm returns informational metadata from the original Primary cut.
// It never refreshes authority or authorizes a read, command or response.
func (r *MemberRequest) OriginalTerm() replication.Term {
	if r == nil || r.original == nil {
		return replication.Term{}
	}
	return r.original.authority.Term
}

// Deadline is the earlier original request, peer certificate and local CA bound.
// Transports install it before body acquisition; it grants no current admission.
func (r *MemberRequest) Deadline() time.Time {
	if r == nil || r.original == nil {
		return time.Time{}
	}
	return r.original.deadline
}

const (
	memberFresh uint32 = iota
	memberActive
	memberComplete
	memberOutput
	memberTerminal
)

type memberRequest struct {
	s         *Store
	ctx       context.Context
	authority replication.Authority
	owner     string
	local     memberidentity.Trust
	peer      *memberidentity.PeerAdmission
	deadline  time.Time
	state     atomic.Uint32
	completed atomic.Pointer[memberCompletion]
}

type memberRequestKey struct{}

type memberCompletion struct {
	authority replication.Authority
	command   bool
}

// CaptureMemberRequest takes ONE bounded read cut of actual Primary authority
// and complete enrollment. Both local and remote members must be active, and
// the actual TLS chain must match that exact remote CA. ctx must be the original
// finite request, at most 30 seconds, established before any body I/O.
func (s *Store) CaptureMemberRequest(ctx context.Context, cluster, member string, connection *tls.ConnectionState) (*MemberRequest, error) {
	if ctx == nil || len(cluster) > 256 || len(member) > 256 || s.isClosed() || s.writeBlocked.Load() {
		return nil, enrollment.ErrRefused
	}
	now := time.Now()
	deadline, finite := ctx.Deadline()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return nil, enrollment.ErrRefused
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return nil, err
	}
	if a.Mode != replication.Primary || !a.Writable() || a.Term.ClusterID != cluster || member == a.MemberID {
		return nil, enrollment.ErrRefused
	}
	if carried, ok := ctx.Value(writerTokenKey{}).(WriterToken); ok && carried != (WriterToken{a.MemberID, a.Term, a.Incarnation}) {
		return nil, replication.ErrFenced
	}
	rows, err := readEnrollmentRows(ctx, tx)
	if err != nil {
		return nil, err
	}
	owner, err := accepted.DecodeRows(rows, cluster)
	if err != nil || owner.State() == nil {
		return nil, enrollment.ErrRefused
	}
	var local, remote memberidentity.Trust
	for _, current := range owner.State().Members() {
		if current.Revoked {
			continue
		}
		if current.ID == a.MemberID {
			local = current.Trust
		}
		if current.ID == member {
			remote = current.Trust
		}
	}
	if !local.ValidAt(now) || !remote.ValidAt(now) {
		return nil, enrollment.ErrRefused
	}
	peer, err := remote.CapturePeer(ctx, connection)
	if err != nil {
		return nil, err
	}
	if peer.Deadline().Before(deadline) {
		deadline = peer.Deadline()
	}
	if local.ExpiresAt().Before(deadline) {
		deadline = local.ExpiresAt()
	}
	original := &memberRequest{s: s, ctx: ctx, authority: a, owner: owner.Digest(), local: local, peer: peer, deadline: deadline}
	if err := original.check(); err != nil {
		return nil, err
	}
	return &MemberRequest{original: original}, nil
}

func sameMemberAuthority(a, b replication.Authority) bool {
	return a.Mode == replication.Primary && b.Mode == replication.Primary && a.MemberID == b.MemberID &&
		a.Incarnation == b.Incarnation && a.Term.Same(b.Term) && a.Writable() && b.Writable()
}

func (r *memberRequest) refuse(err error) error { r.state.Store(memberTerminal); return err }

func (r *memberRequest) check() error {
	if r == nil || r.s == nil || r.ctx == nil || r.peer == nil || r.state.Load() == memberTerminal {
		return enrollment.ErrRefused
	}
	if err := r.peer.Check(); err != nil {
		return r.refuse(err)
	}
	if !r.local.ValidAt(time.Now()) || r.s.isClosed() || r.s.writeBlocked.Load() || !sameMemberAuthority(r.authority, r.s.Authority()) {
		return r.refuse(replication.ErrFenced)
	}
	return nil
}

func (r *MemberRequest) claim() (*memberRequest, error) {
	if r == nil || r.original == nil {
		return nil, enrollment.ErrRefused
	}
	o := r.original
	if err := o.check(); err != nil {
		return nil, err
	}
	if !o.state.CompareAndSwap(memberFresh, memberActive) {
		return nil, enrollment.ErrRefused
	}
	return o, nil
}

func (r *memberRequest) checkOwner(ctx context.Context, tx *sql.Tx) error {
	if err := r.check(); err != nil {
		return err
	}
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return r.refuse(err)
	}
	if !sameMemberAuthority(a, r.authority) {
		return r.refuse(replication.ErrFenced)
	}
	rows, err := readEnrollmentRows(ctx, tx)
	if err != nil {
		return r.refuse(err)
	}
	owner, err := accepted.DecodeRows(rows, r.authority.Term.ClusterID)
	if err != nil {
		return r.refuse(err)
	}
	if owner.Digest() != r.owner {
		return r.refuse(replication.ErrCAS)
	}
	return r.check()
}

func (r *memberRequest) complete(a replication.Authority, command bool) {
	r.completed.Store(&memberCompletion{authority: a, command: command})
	r.state.CompareAndSwap(memberActive, memberComplete)
}

// Peer requests cannot outwait their original certificate/request while queued
// behind another emission. The existing writer lock is unchanged; only these
// bounded requests use context-aware admission, without an abandoned goroutine.
func (r *memberRequest) lock(original context.Context) error {
	if err := original.Err(); err != nil {
		return r.refuse(err)
	}
	if err := r.check(); err != nil {
		return err
	}
	if r.s.wmu.TryLock() {
		return nil
	}
	ctx, cancel := context.WithDeadline(original, r.deadline)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return r.refuse(ctx.Err())
		case <-tick.C:
			if err := r.check(); err != nil {
				return err
			}
			if r.s.wmu.TryLock() {
				return nil
			}
		}
	}
}

// ReadImage acquires one complete image under original membership and writer
// admission. The returned bytes remain private until WithOutput succeeds; a
// newer generation cannot rescue this image or the original certificate.
func (r *MemberRequest) ReadImage() (_ *replication.Image, result error) {
	o, err := r.claim()
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			o.state.Store(memberTerminal)
		}
	}()
	if err := o.lock(o.ctx); err != nil {
		return nil, err
	}
	defer o.s.wmu.Unlock()
	tx, err := o.s.r.BeginTx(o.ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := o.checkOwner(o.ctx, tx); err != nil {
		return nil, err
	}
	image, err := o.s.readGeneration(o.ctx, tx)
	if err != nil {
		return nil, err
	}
	if _, err := o.s.ValidateImage(o.ctx, *image); err != nil {
		return nil, err
	}
	a, err := readAuthority(o.ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := o.check(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	o.complete(a, false)
	return image, nil
}

// MemberCommand carries the captured writer into the actual transaction; it
// never samples a replacement WriterToken. Domain authentication, authorization,
// mutation and dedup must use fn's CommandTx. Callers retain their original
// external dependency exclusions through completion and subsequent output.
func (s *Store) MemberCommand(ctx context.Context, r *MemberRequest, fn func(context.Context, *CommandTx) error) error {
	if r == nil || r.original == nil || r.original.s != s || ctx == nil || fn == nil {
		return enrollment.ErrRefused
	}
	o, err := r.claim()
	if err != nil {
		return err
	}
	writer := WriterToken{o.authority.MemberID, o.authority.Term, o.authority.Incarnation}
	if carried, ok := ctx.Value(writerTokenKey{}).(WriterToken); ok && carried != writer {
		return o.refuse(replication.ErrFenced)
	}
	// Keep incoming request values, but neither an independent context nor a
	// later deadline can detach cancellation from the original peer operation.
	ctx, cancel := context.WithDeadline(ctx, o.deadline)
	joined := make(chan struct{})
	stop := context.AfterFunc(o.ctx, func() { cancel(); close(joined) })
	defer func() {
		if !stop() {
			<-joined
		}
		cancel()
	}()
	ctx = context.WithValue(WithWriterToken(ctx, writer), memberRequestKey{}, o)
	err = s.Command(ctx, o.authority.Term, fn)
	if err != nil {
		o.state.Store(memberTerminal)
	}
	return err
}

func (t *CommandTx) enterMember(ctx context.Context) error {
	if t.member == nil {
		return nil
	}
	r := t.member
	if r.s != t.s || r.state.Load() != memberActive {
		return t.poison(enrollment.ErrRefused)
	}
	if err := r.checkOwner(ctx, t.tx); err != nil {
		return t.poison(err)
	}
	return t.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix)
}

// WithOutput runs once after an actual successful image read or command. It
// compares the exact resulting authority/generation and original owner under
// wmu and holds that exclusion through emit. Acquire all domain/dependency pins
// BEFORE this call. emit may perform bounded transport I/O ONLY: no Store calls,
// domain/publication lock acquisition, lookup, goroutines or resource release.
// The supplied context preserves original cancellation and the earlier captured
// request/peer-certificate/local-CA deadline. emit must install that deadline on
// the actual transport and explicitly flush before returning.
// A callback error or panic consumes this output; cancellation never releases
// the exclusion while emit is still running. This is no domain permission grant.
func (r *MemberRequest) WithOutput(emit func(context.Context) error) error {
	return r.withOutput(false, emit)
}

// WithCommandOutput has WithOutput's lifetime, locking and transport contract,
// but requires actual successful MemberCommand completion. An image receipt
// cannot authorize a command acknowledgment, including through a copied handle.
func (r *MemberRequest) WithCommandOutput(emit func(context.Context) error) error {
	return r.withOutput(true, emit)
}

func (r *MemberRequest) withOutput(requireCommand bool, emit func(context.Context) error) error {
	if r == nil || r.original == nil || emit == nil {
		return enrollment.ErrRefused
	}
	o := r.original
	if !o.state.CompareAndSwap(memberComplete, memberOutput) {
		return enrollment.ErrRefused
	}
	defer o.state.Store(memberTerminal)
	if err := o.lock(o.ctx); err != nil {
		return err
	}
	defer o.s.wmu.Unlock()
	if err := o.check(); err != nil {
		return err
	}
	completed := o.completed.Load()
	if completed == nil || requireCommand && !completed.command || !reflect.DeepEqual(o.s.Authority(), completed.authority) {
		return replication.ErrCAS
	}
	tx, err := o.s.r.BeginTx(o.ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := o.checkOwner(o.ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := o.check(); err != nil {
		return err
	}
	outputCtx, cancel := context.WithDeadline(o.ctx, o.deadline)
	defer cancel()
	if err := emit(outputCtx); err != nil {
		return err
	}
	return o.check()
}
