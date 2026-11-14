package memberidentity

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"time"
)

// PeerAdmission retains one actual TLS peer verification and its original
// certificate/request lifetime. It grants no membership or domain permission.
// Copies share terminal state; caller-owned TLS fields are never reread.
type PeerAdmission struct{ life *peerLife }

type peerLife struct {
	ctx     context.Context
	expires time.Time
	last    atomic.Int64
	closed  atomic.Bool
}

// Deadline is the earlier original request/certificate deadline. Callers use it
// to bound actual transport emission; it does not make an admission current.
func (a *PeerAdmission) Deadline() time.Time {
	if a == nil || a.life == nil {
		return time.Time{}
	}
	deadline, finite := a.life.ctx.Deadline()
	if !finite {
		return time.Time{}
	}
	if a.life.expires.Before(deadline) {
		return a.life.expires
	}
	return deadline
}

// CapturePeer verifies the transport's actual completed TLS state against this
// exact enrolled trust before application body I/O. The original request must
// have a finite deadline no more than 30 seconds away. A new trust or TLS state
// cannot replace the captured certificate later in the request.
func (t Trust) CapturePeer(ctx context.Context, state *tls.ConnectionState) (*PeerAdmission, error) {
	if ctx == nil {
		return nil, ErrIdentity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	deadline, finite := ctx.Deadline()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return nil, ErrIdentity
	}
	expires, err := t.peerExpiry(state, now)
	if err != nil {
		return nil, err
	}
	life := &peerLife{ctx: ctx, expires: expires}
	life.last.Store(now.UnixNano())
	a := &PeerAdmission{life: life}
	if err := a.Check(); err != nil {
		return nil, err
	}
	return a, nil
}

// Check inspects only original immutable evidence, cancellation and the actual
// clock. Failure is terminal, including clock rollback and copied handles.
func (a *PeerAdmission) Check() error {
	if a == nil || a.life == nil || a.life.closed.Load() {
		return ErrIdentity
	}
	life := a.life
	fail := func(err error) error { life.closed.Store(true); return err }
	if err := life.ctx.Err(); err != nil {
		return fail(err)
	}
	now := time.Now()
	deadline, finite := life.ctx.Deadline()
	if !finite || !now.Before(deadline) {
		return fail(context.DeadlineExceeded)
	}
	if !now.Before(life.expires) {
		return fail(ErrIdentity)
	}
	nanos := now.UnixNano()
	if !time.Unix(0, nanos).Equal(now) {
		return fail(ErrIdentity)
	}
	for {
		previous := life.last.Load()
		if nanos < previous {
			return fail(ErrIdentity)
		}
		if life.last.CompareAndSwap(previous, nanos) {
			break
		}
	}
	if err := life.ctx.Err(); err != nil {
		return fail(err)
	}
	if life.closed.Load() {
		return ErrIdentity
	}
	return nil
}
