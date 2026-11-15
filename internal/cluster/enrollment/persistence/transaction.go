// Package persistence stages complete enrollment transitions inside an original
// SQLite Command. It acquires no replacement writer or operator authorization.
// Every returned error must abort the outer command. Callers retain their
// authorization dependency exclusion until that command actually completes.
package persistence

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Original binds a single use to the original writer, complete owner and finite
// authorization lifetime. Final callbacks may inspect retained memory/clock only.
type Original struct {
	ctx       context.Context
	writer    sqlite.WriterToken
	expected  string
	authorize func() error
	at        time.Time
	claim     *originalClaim
}

type originalClaim struct {
	used   atomic.Bool
	closed atomic.Bool
}

// Capture must precede remote acquisition. Authorization is an original check,
// not a callback that can reacquire policy or perform I/O at commit.
func Capture(ctx context.Context, expected accepted.Owner, authorize func() error) (*Original, error) {
	if ctx == nil || expected.Digest() == "" || authorize == nil {
		return nil, enrollment.ErrRefused
	}
	writer, ok := sqlite.WriterTokenFromContext(ctx)
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !ok || !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second || expected.ClusterID() != writer.Term().ClusterID {
		return nil, enrollment.ErrRefused
	}
	o := &Original{ctx: ctx, writer: writer, expected: expected.Digest(), authorize: authorize, at: now, claim: &originalClaim{}}
	if err := o.check(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Original) check(ctx context.Context) error {
	if o == nil || o.claim == nil || o.ctx == nil || ctx == nil || o.authorize == nil || o.at.IsZero() || o.claim.closed.Load() {
		return enrollment.ErrRefused
	}
	fail := func(err error) error { o.claim.closed.Store(true); return err }
	now := time.Now()
	if now.UTC().Before(o.at.UTC()) {
		return fail(enrollment.ErrRefused)
	}
	for _, c := range []context.Context{o.ctx, ctx} {
		if err := c.Err(); err != nil {
			return fail(err)
		}
		deadline, finite := c.Deadline()
		if !finite || !now.Before(deadline) {
			return fail(context.DeadlineExceeded)
		}
		writer, ok := sqlite.WriterTokenFromContext(c)
		if !ok || writer != o.writer {
			return fail(enrollment.ErrRefused)
		}
	}
	if err := o.authorize(); err != nil {
		return fail(err)
	}
	return nil
}

func (o *Original) enter(ctx context.Context, tx *sqlite.CommandTx) (accepted.Owner, error) {
	if tx == nil {
		return accepted.Owner{}, sqlite.ErrTransactionEnded
	}
	if err := o.check(ctx); err != nil {
		return accepted.Owner{}, err
	}
	if !o.claim.used.CompareAndSwap(false, true) || !tx.Authority().Same(o.writer.Term()) || tx.Incarnation() != o.writer.Incarnation() {
		o.claim.closed.Store(true)
		return accepted.Owner{}, enrollment.ErrRefused
	}
	current, err := Load(ctx, tx)
	if err != nil {
		o.claim.closed.Store(true)
		return accepted.Owner{}, err
	}
	if current.Digest() != o.expected {
		o.claim.closed.Store(true)
		return accepted.Owner{}, replication.ErrCAS
	}
	if err := tx.RequireCommitCheck(o.check); err != nil {
		return accepted.Owner{}, err
	}
	return current, nil
}

// Staged describes a proposal only. It becomes usable after actual outer command
// success; no caller may treat this digest as a committed enrollment receipt.
type Staged struct{ ProposedDigest string }

func loadRows(ctx context.Context, tx *sqlite.CommandTx) ([]replication.Entry, error) {
	if tx == nil {
		return nil, sqlite.ErrTransactionEnded
	}
	if err := tx.CheckSectionPrefixBounds(ctx, accepted.Section, accepted.Prefix, 1, accepted.MaxRowBytes); err != nil {
		return nil, err
	}
	return tx.ListSectionPrefix(ctx, accepted.Section, accepted.Prefix)
}

// Load validates the actual complete owner, with bounds before allocation.
func Load(ctx context.Context, tx *sqlite.CommandTx) (accepted.Owner, error) {
	rows, err := loadRows(ctx, tx)
	if err != nil {
		return accepted.Owner{}, err
	}
	return accepted.DecodeRows(rows, tx.Authority().ClusterID)
}

// InitializeDormant converts only the physical migration sentinel. It grants no
// enrollment and never interprets missing or corrupt history as a fresh owner.
func InitializeDormant(ctx context.Context, tx *sqlite.CommandTx) error {
	rows, err := loadRows(ctx, tx)
	if err != nil {
		return err
	}
	if !accepted.IsAwaiting(rows) {
		if _, err := accepted.DecodeRows(rows, tx.Authority().ClusterID); err != nil {
			return err
		}
		return tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix)
	}
	next, err := accepted.Dormant(tx.Authority().ClusterID)
	if err != nil {
		return err
	}
	_, err = stage(ctx, tx, next)
	return err
}

// Activate explicitly initializes the first member from an existing dormant
// owner. The member must be the actual original writer's local identity.
func Activate(ctx context.Context, tx *sqlite.CommandTx, o *Original, first enrollment.Member) (Staged, error) {
	current, err := o.enter(ctx, tx)
	if err != nil {
		return Staged{}, err
	}
	if current.State() != nil || first.ID != tx.Authority().PrimaryID {
		return Staged{}, enrollment.ErrRefused
	}
	s, err := enrollment.Initialize(current.ClusterID(), first, time.Now())
	if err != nil {
		return Staged{}, err
	}
	// Initial CA expiry is retained through final admission too.
	if err := tx.RequireCommitCheck(func(context.Context) error {
		if !first.Trust.ValidAt(time.Now()) {
			return enrollment.ErrRefused
		}
		return nil
	}); err != nil {
		return Staged{}, err
	}
	return stageState(ctx, tx, s)
}

func issuer(tx *sqlite.CommandTx) enrollment.Issuer {
	t := tx.Authority()
	return enrollment.Issuer{MemberID: t.PrimaryID, Epoch: t.Epoch, PromotionID: t.PromotionID}
}

// Issue stages the hash-only invitation. Returned secret bytes may be delivered
// only after actual command success under the original operator output scope.
func Issue(ctx context.Context, tx *sqlite.CommandTx, o *Original, name, address string, expires time.Time) (Staged, enrollment.Invitation, error) {
	current, err := o.enter(ctx, tx)
	if err != nil {
		return Staged{}, enrollment.Invitation{}, err
	}
	next, inv, err := current.State().Issue(issuer(tx), name, address, time.Now(), expires)
	if err != nil {
		return Staged{}, enrollment.Invitation{}, err
	}
	_, _, trust := inv.IssuingPeer()
	if err := tx.RequireCommitCheck(func(context.Context) error {
		now := time.Now()
		if !now.Before(expires) || !trust.ValidAt(now) {
			return enrollment.ErrRefused
		}
		return nil
	}); err != nil {
		return Staged{}, enrollment.Invitation{}, err
	}
	staged, err := stageState(ctx, tx, next)
	if err != nil {
		return Staged{}, enrollment.Invitation{}, err
	}
	return staged, inv, nil
}

// Accept verifies possession once against this actual owner and consumes the
// invitation with member addition. Its opaque original admission survives to
// the final commit check; later callback changes cannot replace either effect.
func Accept(ctx context.Context, tx *sqlite.CommandTx, o *Original, proposal enrollment.Proposal) (Staged, error) {
	current, err := o.enter(ctx, tx)
	if err != nil {
		return Staged{}, err
	}
	actualIssuer := issuer(tx)
	a, err := current.State().Admit(o.ctx, proposal, actualIssuer)
	if err != nil {
		return Staged{}, err
	}
	before, err := current.State().Digest()
	if err != nil {
		return Staged{}, err
	}
	after, err := a.State().Digest()
	if err != nil {
		return Staged{}, err
	}
	staged, err := stageState(ctx, tx, a.State())
	if err != nil {
		return Staged{}, err
	}
	if err := tx.RequireCommitCheck(func(context.Context) error { return a.Check(before, after, actualIssuer) }); err != nil {
		return Staged{}, err
	}
	return staged, nil
}

// Revoke retains permanent member/key history and cancels old target tickets.
func Revoke(ctx context.Context, tx *sqlite.CommandTx, o *Original, id string) (Staged, error) {
	current, err := o.enter(ctx, tx)
	if err != nil {
		return Staged{}, err
	}
	next, err := current.State().Revoke(id, time.Now())
	if err != nil {
		return Staged{}, err
	}
	return stageState(ctx, tx, next)
}

// PruneInvitations reclaims only expired invitations, never member history.
func PruneInvitations(ctx context.Context, tx *sqlite.CommandTx, o *Original) (Staged, error) {
	current, err := o.enter(ctx, tx)
	if err != nil {
		return Staged{}, err
	}
	next, err := current.State().PruneInvitations(time.Now())
	if err != nil {
		return Staged{}, err
	}
	return stageState(ctx, tx, next)
}

func stageState(ctx context.Context, tx *sqlite.CommandTx, state *enrollment.State) (Staged, error) {
	next, err := accepted.Enrolled(state)
	if err != nil {
		return Staged{}, err
	}
	at := state.UpdatedAt()
	if err := tx.RequireCommitCheck(func(context.Context) error {
		if time.Now().Before(at) {
			return enrollment.ErrRefused
		}
		return nil
	}); err != nil {
		return Staged{}, err
	}
	return stage(ctx, tx, next)
}

func stage(ctx context.Context, tx *sqlite.CommandTx, next accepted.Owner) (Staged, error) {
	rows, err := next.Rows()
	if err != nil {
		return Staged{}, err
	}
	if err := tx.PutSection(ctx, accepted.Section, rows[0]); err != nil {
		return Staged{}, err
	}
	if err := tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix); err != nil {
		return Staged{}, err
	}
	return Staged{ProposedDigest: next.Digest()}, nil
}
